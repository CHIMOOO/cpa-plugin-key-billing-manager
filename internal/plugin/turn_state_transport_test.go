package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestProtectedStateWebsocketHTTPBridgeChecksEveryTurn(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	if err := a.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["dummy-a"],"models":["model"]}`)); err != nil {
		t.Fatal(err)
	}
	a.turnState.Before("learn", "dummy-a", "model", nil)
	token := rpcTurnStateTemplate()
	if err := a.turnState.Learn("learn", "dummy-a", "model", http.Header{turnstate.Header: {token}}); err != nil {
		t.Fatal(err)
	}
	websockets, reads := false, 0
	a.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.get_runtime" {
			t.Fatalf("unexpected callback %s", method)
		}
		if payload.(map[string]string)["auth_index"] != "index-a" {
			t.Fatal("runtime identity was not pinned")
		}
		reads++
		return mustMarshal(t, map[string]any{"auth": map[string]any{"id": "dummy-a", "auth_index": "index-a", "provider": "codex", "websockets": websockets}}), nil
	})
	call := func(id string, headers http.Header, session string) RequestInterceptResponse {
		t.Helper()
		metadata := map[string]any{MetadataSelectedAuth: "dummy-a", MetadataSelectedIndex: "index-a", MetadataCallerScope: flowScope()}
		if session != "" {
			metadata["execution_session_id"] = session
		}
		raw, err := a.interceptAfterAuth(mustMarshal(t, RequestInterceptRequest{RequestID: id, Model: "model", Headers: headers, Metadata: metadata}))
		if err != nil {
			t.Fatal(err)
		}
		var result RequestInterceptResponse
		decodeResult(t, raw, &result)
		return result
	}
	for _, req := range []struct {
		id      string
		headers http.Header
		session string
	}{
		{"first", http.Header{"Upgrade": {"websocket"}}, ""},
		{"second", nil, "same-downstream-session"},
	} {
		got := call(req.id, req.headers, req.session)
		if got.Terminate || got.Headers.Get(turnstate.Header) != token {
			t.Fatalf("HTTP bridge rejected fresh state: %+v", got)
		}
	}
	if reads != 2 {
		t.Fatalf("runtime reads=%d, want one per protected WS turn", reads)
	}
	websockets = true
	got := call("external-change", nil, "same-downstream-session")
	if !got.Terminate || got.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(got.ResponseBody), "turn_state_websocket_unsupported") {
		t.Fatalf("external upstream WS enablement bypassed protection: %+v", got)
	}
	got = call("plain-http", nil, "")
	if got.Terminate || got.Headers.Get(turnstate.Header) != token || reads != 3 {
		t.Fatalf("plain HTTP acquired an unnecessary runtime dependency: result=%+v reads=%d", got, reads)
	}
}

func TestTurnStateUpstreamHTTPRequiresExactRuntimeIdentity(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	req := RequestInterceptRequest{Metadata: map[string]any{MetadataSelectedIndex: "index-a"}}
	for _, test := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{"explicit-false", `{"auth":{"id":"dummy-a","auth_index":"index-a","provider":"codex","websockets":false}}`, true},
		{"omitted-false", `{"auth":{"id":"dummy-a","auth_index":"index-a","type":"codex"}}`, true},
		{"enabled", `{"auth":{"id":"dummy-a","auth_index":"index-a","provider":"codex","websockets":true}}`, false},
		{"wrong-account", `{"auth":{"id":"dummy-b","auth_index":"index-a","provider":"codex"}}`, false},
		{"wrong-index", `{"auth":{"id":"dummy-a","auth_index":"index-b","provider":"codex"}}`, false},
		{"wrong-provider", `{"auth":{"id":"dummy-a","auth_index":"index-a","provider":"xai"}}`, false},
		{"missing-auth", `{}`, false},
		{"malformed-mode", `{"auth":{"id":"dummy-a","auth_index":"index-a","provider":"codex","websockets":"false"}}`, false},
		{"null-mode", `{"auth":{"id":"dummy-a","auth_index":"index-a","provider":"codex","websockets":null}}`, false},
		{"malformed-json", `invalid`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a.SetHostCaller(func(string, any) (json.RawMessage, error) { return []byte(test.raw), nil })
			if got := a.turnStateUpstreamHTTP(req, "dummy-a"); got != test.ok {
				t.Fatalf("verified=%v, want %v", got, test.ok)
			}
		})
	}
	a.SetHostCaller(func(string, any) (json.RawMessage, error) { return nil, errors.New("dummy unavailable") })
	if a.turnStateUpstreamHTTP(req, "dummy-a") {
		t.Fatal("unavailable runtime was accepted")
	}
	a.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Fatal("unsupported identity called host")
		return nil, nil
	})
	if a.turnStateUpstreamHTTP(RequestInterceptRequest{}, "dummy-a") {
		t.Fatal("missing index accepted")
	}
	a.hostSchema.Store(5)
	if a.turnStateUpstreamHTTP(req, "dummy-a") {
		t.Fatal("unverified host accepted")
	}
}

func TestTurnStateStatusOffersNarrowWebsocketPatchesWithoutWriting(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	a.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("status attempted a mutation or credential read: %s", method)
		}
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "dummy-a", AuthIndex: "index-a", Provider: "codex", Source: "file", Websockets: true},
			{ID: "dummy-b", AuthIndex: "index-b", Provider: "codex", Source: "file"},
			{ID: "dummy-runtime", AuthIndex: "index-r", Provider: "codex", RuntimeOnly: true},
			{ID: "dummy-other", AuthIndex: "index-o", Provider: "claude", Source: "file"},
		}}), nil
	})
	var status turnStateStatus
	if err := json.Unmarshal(a.getTurnState(ManagementRequest{}).Body, &status); err != nil {
		t.Fatal(err)
	}
	if !status.UpstreamWebsocketManagementSupported || status.UpstreamWebsocketPatchPath != "/v0/management/auth-files/fields" || len(status.ProbeAccounts) != 3 {
		t.Fatalf("invalid transport status: %+v", status)
	}
	for i, row := range status.ProbeAccounts {
		if !row.UpstreamTransportKnown || row.UpstreamWebsockets != (i == 0) {
			t.Fatalf("incorrect mode: %+v", row)
		}
		if i < 2 {
			if row.DisableWebsocketsPatch == nil || row.DisableWebsocketsPatch.Name != row.Account || row.DisableWebsocketsPatch.Websockets {
				t.Fatalf("patch must affect only the selected account transport: %+v", row)
			}
		} else if row.DisableWebsocketsPatch != nil {
			t.Fatal("runtime-only credential offered a file patch")
		}
	}
	a.hostSchema.Store(4)
	status = turnStateStatus{}
	if err := json.Unmarshal(a.getTurnState(ManagementRequest{}).Body, &status); err != nil {
		t.Fatal(err)
	}
	if status.UpstreamWebsocketManagementSupported {
		t.Fatal("legacy host was advertised as verified")
	}
	for _, row := range status.ProbeAccounts {
		if row.UpstreamTransportKnown || row.DisableWebsocketsPatch != nil {
			t.Fatal("legacy host mode was trusted or offered a patch")
		}
	}
}

func TestRegistrationNegotiatesUnusedStreamHistory(t *testing.T) {
	a := newConfiguredApp(t)
	for _, host := range []uint32{0, 4, 5, 6, 9} {
		raw, err := a.HandleMethod(MethodPluginReconfigure, mustMarshal(t, LifecycleRequest{
			ConfigYAML: testConfigYAML(t, true), SchemaVersion: host,
		}))
		if err != nil {
			t.Fatal(err)
		}
		var got Registration
		decodeResult(t, raw, &got)
		want := uint32(4)
		if host >= 5 {
			want = 5
		}
		if got.SchemaVersion != want || !got.Capabilities.StreamChunkInterceptor || a.hostSchema.Load() != host {
			t.Fatalf("host=%d registration=%+v", host, got)
		}
	}
}
