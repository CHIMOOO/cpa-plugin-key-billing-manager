package plugin

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func rpcTurnStateTemplate() string {
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(time.Now().Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func turnStateAfterAuth(t *testing.T, app *App, id, account, model string, headers http.Header) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
		RequestID: id, ToFormat: "codex", Model: model, RequestedModel: "client-alias", Headers: headers,
		Metadata: map[string]any{MetadataSelectedAuth: account, MetadataCallerScope: flowScope()},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

func TestTurnStateHooksLearnAndInjectWithoutChangingResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			app := newConfiguredApp(t)
			app.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
				return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
					{ID: "dummy-auth-a", AuthIndex: "dummy-index-a", Provider: "codex", Type: "codex", Source: "file"},
					{ID: "dummy-auth-b", AuthIndex: "dummy-index-b", Provider: "codex", Type: "codex", Source: "file"},
				}}), nil
			})
			if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["dummy-auth-a"],"models":["upstream-model"]}`)); err != nil {
				t.Fatal(err)
			}
			turnStateAfterAuth(t, app, "learning", "dummy-auth-a", "upstream-model", nil)
			token := rpcTurnStateTemplate()
			method := MethodResponseInterceptAfter
			if stream {
				method = MethodResponseStreamChunk
			}
			raw, err := app.HandleMethod(method, mustMarshal(t, map[string]any{
				"RequestID": "learning", "Model": "client-alias", "ChunkIndex": -1,
				"ResponseHeaders": http.Header{turnstate.Header: {token}},
				"Body":            []byte("original upstream payload"),
			}))
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			decodeResult(t, raw, &result)
			if result["Body"] != nil || result["DropChunk"] == true {
				t.Fatalf("header observation changed upstream payload: %+v", result)
			}
			response := turnStateAfterAuth(t, app, "using", "dummy-auth-a", "upstream-model", nil)
			if response.Headers.Get(turnstate.Header) != token || response.Terminate {
				t.Fatalf("template not injected through RPC: %+v", response)
			}
			for _, pair := range [][2]string{{"dummy-auth-b", "upstream-model"}, {"dummy-auth-a", "client-alias"}} {
				if got := turnStateAfterAuth(t, app, "isolated", pair[0], pair[1], nil); len(got.Headers) != 0 {
					t.Fatalf("template crossed account/model boundary: %+v", got)
				}
			}
			if err := app.turnState.Update([]byte(`{"inject_mode":"replace-only"}`)); err != nil {
				t.Fatal(err)
			}
			if got := turnStateAfterAuth(t, app, "pass", "dummy-auth-a", "upstream-model", nil); len(got.Headers) != 0 {
				t.Fatal("replace-only injected absent header")
			}
			if got := turnStateAfterAuth(t, app, "replace", "dummy-auth-a", "upstream-model", http.Header{turnstate.Header: {strings.Repeat("x", 312)}}); got.Headers.Get(turnstate.Header) != token {
				t.Fatal("replace-only did not replace degraded header")
			}
		})
	}
}

func TestTurnStateDoesNotBypassCredentialRefusal(t *testing.T) {
	app := newConfiguredApp(t)
	if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
		t.Fatal(err)
	}
	if err := app.store.SetAccessControl(billing.AccessControl{Enabled: true, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	response := turnStateAfterAuth(t, app, "denied", "dummy-auth-a", "upstream-model", nil)
	if !response.Terminate || len(response.Headers) != 0 {
		t.Fatalf("turn-state bypassed access control: %+v", response)
	}
}

func TestTurnStateSidecarFailureKeepsBillingConfiguration(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "other.db")
	if err := os.WriteFile(path+".turn-state.json", []byte("invalid state"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := app.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: false\nstate_file: " + strconv.Quote(path) + "\n"),
	}))
	if err == nil || !app.store.Enabled() || !app.turnState.Enabled() {
		t.Fatalf("invalid sidecar partially applied configuration: err=%v, billing=%v, turn-state=%v", err, app.store.Enabled(), app.turnState.Enabled())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("billing database created before sidecar validation: %v", err)
	}
}
