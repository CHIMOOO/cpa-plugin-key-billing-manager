package plugin

import (
	"encoding/json"
	"net/http"
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

func TestTurnStateProbeOmitsDisabledAccountsAndKeepsRecoveryProbes(t *testing.T) {
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
	if len(status.ProbeAccounts) != 2 || status.ProbeAccounts[0].Account != "enabled" || status.ProbeAccounts[1].Account != "unavailable" {
		t.Fatalf("probe account list bypassed availability: %+v", status.ProbeAccounts)
	}
	if len(status.Config.ProbeAccounts) != 3 {
		t.Fatal("status silently rewrote saved selection")
	}
	if codexAuthFile(hostAuthFile{ID: "disabled", AuthIndex: "x", Provider: "codex", Disabled: true}) {
		t.Fatal("disabled account remained probeable")
	}
}

func TestTurnStateProbeRejectsSavedDisabledOrDeletedAccount(t *testing.T) {
	if !turnstate.ProbeSupported() {
		t.Skip("curl is not available")
	}
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"probe_accounts":["disabled","deleted"],"models":["dummy-model"]}`)); err != nil {
		t.Fatal(err)
	}
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("probe fetched a disabled/deleted account's OAuth token: %q", method)
		}
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "disabled", AuthIndex: "disabled-index", Provider: "codex", Status: "disabled"},
		}}), nil
	})
	response := app.probeTurnState(ManagementRequest{Body: []byte(`{}`)})
	var result turnstate.ProbeResult
	if err := json.Unmarshal(response.Body, &result); err != nil || response.StatusCode != http.StatusOK || result.Action != "error" {
		t.Fatalf("probe did not reject unavailable selection: %s, err=%v", response.Body, err)
	}
}
