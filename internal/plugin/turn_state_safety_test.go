package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func TestTurnStateIgnoresNonCodexAndClearsRetryAttribution(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	for account, provider := range map[string]string{"dummy-codex": "codex", "dummy-xai": "xai"} {
		ref := billing.CredentialFingerprint(account)
		app.credentials[ref] = credentialView{Ref: ref, Provider: provider, Source: billing.CredentialSourceAuthFiles}
	}
	// XAI shares ToFormat=codex, so the format must not authorize learning.
	turnStateAfterAuth(t, app, "retry", "dummy-codex", "model-a", nil)
	turnStateAfterAuth(t, app, "retry", "dummy-xai", "model-a", nil)
	_, err := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, map[string]any{
		"RequestID": "retry", "Model": "model-a", "ResponseHeaders": http.Header{turnstate.Header: {rpcTurnStateTemplate()}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if rows := app.turnState.Status().Templates; len(rows) != 0 {
		t.Fatalf("non-Codex retry learned a template under the previous account: %+v", rows)
	}
}

func TestTurnStateDisabledDoesNotReadHostCredentials(t *testing.T) {
	app := newConfiguredApp(t)
	app.hostCaller = func(string, any) (json.RawMessage, error) {
		t.Fatal("disabled turn-state read host credentials")
		return nil, nil
	}
	if got := turnStateAfterAuth(t, app, "disabled", "dummy-codex", "model-a", nil); len(got.Headers) != 0 {
		t.Fatal("disabled turn-state rewrote request")
	}
}

func TestTurnStateRequiresActualUpstreamModel(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	ref := billing.CredentialFingerprint("dummy-codex")
	app.credentials[ref] = credentialView{Ref: ref, Provider: "codex"}
	turnStateAfterAuth(t, app, "missing-model", "dummy-codex", "", nil)
	_, err := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, map[string]any{
		"RequestID": "missing-model", "Model": "client-alias", "ResponseHeaders": http.Header{turnstate.Header: {rpcTurnStateTemplate()}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(app.turnState.Status().Templates) != 0 {
		t.Fatal("client alias used to guess missing upstream model")
	}
}

func TestTurnStateProbeIncludesDisabledAccountsAndKeepsRecoveryProbes(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"probe_accounts":["enabled","disabled","unavailable"]}`)); err != nil {
		t.Fatal(err)
	}
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("host method = %q", method)
		}
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "enabled", AuthIndex: "enabled-index", Provider: "codex"},
			{ID: "disabled", AuthIndex: "disabled-index", Provider: "codex", Disabled: true},
			{ID: "status-disabled", AuthIndex: "status-disabled-index", Provider: "codex", Status: " DISABLED "},
			{ID: "unavailable", AuthIndex: "unavailable-index", Provider: "codex", Unavailable: true},
			{ID: "xai", AuthIndex: "xai-index", Provider: "xai"},
			{ID: "missing-index", Provider: "codex"},
			{AuthIndex: "missing-id", Provider: "codex"},
		}}), nil
	})
	response := app.getTurnState(ManagementRequest{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var status turnStateStatus
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"enabled": false, "disabled": true, "status-disabled": true, "unavailable": false}
	got := map[string]bool{}
	for _, account := range status.ProbeAccounts {
		got[account.Account] = account.Disabled
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probe account list omitted eligible recovery accounts: %+v", status.ProbeAccounts)
	}
	if len(status.Config.ProbeAccounts) != 3 {
		t.Fatal("status silently rewrote saved selection")
	}
	if status.HostRequirementMessage.Key != "backend.turn_state_host_requirement" {
		t.Fatalf("host requirement has no translation metadata: %+v", status.HostRequirementMessage)
	}
}

func TestTurnStateManagementValidationHasTranslationMetadata(t *testing.T) {
	app := newConfiguredApp(t)
	response := app.setTurnState(ManagementRequest{Body: []byte(`{"inject_mode":"invalid"}`)})
	var body struct {
		Error struct {
			Key string `json:"message_key"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil || response.StatusCode != http.StatusBadRequest || body.Error.Key != "backend.turn_state_invalid_inject_mode" {
		t.Fatalf("turn-state validation metadata = %s, err=%v", response.Body, err)
	}
}

func TestTurnStateProbeRejectsSavedDeletedOrUnsupportedAccount(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"probe_accounts":["xai","deleted"],"models":["dummy-model"]}`)); err != nil {
		t.Fatal(err)
	}
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("probe fetched a deleted/unsupported account's OAuth token: %q", method)
		}
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "xai", AuthIndex: "xai-index", Provider: "xai"},
		}}), nil
	})
	response := app.probeTurnState(ManagementRequest{Body: []byte(`{}`)})
	var result turnstate.ProbeResult
	if err := json.Unmarshal(response.Body, &result); err != nil || response.StatusCode != http.StatusOK || result.Action != "error" {
		t.Fatalf("probe did not reject unavailable selection: %s, err=%v", response.Body, err)
	}
}

func TestTurnStateProbeDisabledAccountNeverChangesHostRouting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusOnly bool
		credential string
	}{
		{"disabled-flag", false, `{"access_token":"dummy-disabled-token","account_id":"dummy-disabled-account"}`},
		{"disabled-status", true, `{"access_token":"dummy-disabled-token","account_id":"dummy-disabled-account"}`},
		{"invalid-credential", false, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A real local proxy exercises the selected exit without contacting
			// the upstream or spending quota. Successful HTTP harvests are covered
			// with a local TLS fixture in turnstate's transport tests.
			var requests atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected probe proxy request: %+v", r)
				}
				w.Header().Set(turnstate.Header, rpcTurnStateTemplate())
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer proxy.Close()
			app := newConfiguredApp(t)
			if err := app.turnState.Update(mustMarshal(t, map[string]any{
				"probe_accounts": []string{"disabled", "enabled"}, "models": []string{"dummy-model"},
				"probe_proxies": []string{proxy.URL, "http://second.invalid:8000"},
			})); err != nil {
				t.Fatal(err)
			}
			files := []hostAuthFile{
				{ID: "disabled", AuthIndex: "disabled-index", Provider: "codex", Disabled: !tc.statusOnly, Status: "disabled"},
				{ID: "enabled", AuthIndex: "enabled-index", Provider: "codex"},
			}
			beforeFiles := append([]hostAuthFile(nil), files...)
			beforeConfig := app.turnState.Status().Config
			reads := 0
			app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case hostAuthList:
					return mustMarshal(t, hostAuthListResponse{Files: files}), nil
				case hostAuthGet:
					reads++
					if !reflect.DeepEqual(payload, map[string]string{"auth_index": "disabled-index"}) {
						t.Fatalf("probe read a different account: %+v", payload)
					}
					return mustMarshal(t, hostAuthGetResponse{JSON: json.RawMessage(tc.credential)}), nil
				default:
					t.Fatalf("probe must not write host account state or proxy configuration: %q", method)
					return nil, nil
				}
			})
			response := app.probeTurnState(ManagementRequest{Body: []byte(`{"account":"disabled","model":"dummy-model"}`)})
			var result turnstate.ProbeResult
			if err := json.Unmarshal(response.Body, &result); err != nil || response.StatusCode != http.StatusOK || result.Action != "error" || result.Account != "disabled" {
				t.Fatalf("disabled account probe = %s, err=%v", response.Body, err)
			}
			if reads != 1 || !reflect.DeepEqual(files, beforeFiles) || !reflect.DeepEqual(app.turnState.Status().Config, beforeConfig) {
				t.Fatal("probe changed account enablement, proxy order, or read more than the selected credential")
			}
			wantRequests := int32(1)
			if tc.credential == `{}` {
				wantRequests = 0
			}
			if requests.Load() != wantRequests {
				t.Fatalf("probe reached the selected proxy %d times; want %d", requests.Load(), wantRequests)
			}
			if rows := app.turnState.Status().Templates; len(rows) != 0 {
				t.Fatalf("failed probe learned a proxy's template: %+v", rows)
			}
		})
	}
}
