package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

// An exclusive group holding a Codex account and a bound common group holding
// an xAI account must serve Grok on the xAI account, including while the Codex
// account is protected by State.
func TestExclusiveCodexGroupWithBoundCommonXAIGroupServesGrok(t *testing.T) {
	codex, xai := billing.CredentialFingerprint("dummy-codex"), billing.CredentialFingerprint("dummy-xai")
	for _, tc := range []struct {
		name            string
		exclusiveModels []string
		commonModels    []string
		protectCodex    bool
		bindCommon      bool
		direct          *billing.RouteRule
		clearDirect     bool
		wantGrok        bool
		wantGPT         bool
	}{
		{name: "both list models", exclusiveModels: []string{"gpt-5.6"}, commonModels: []string{"grok-4"}, bindCommon: true, wantGrok: true, wantGPT: true},
		{name: "exclusive lists no models", commonModels: []string{"grok-4"}, bindCommon: true, wantGrok: true, wantGPT: true},
		{name: "common lists no models", exclusiveModels: []string{"gpt-5.6"}, bindCommon: true, wantGrok: true, wantGPT: true},
		{name: "codex protected by State", exclusiveModels: []string{"gpt-5.6"}, commonModels: []string{"grok-4"}, protectCodex: true, bindCommon: true, wantGrok: true},
		{name: "key rule cleared", exclusiveModels: []string{"gpt-5.6"}, commonModels: []string{"grok-4"}, bindCommon: true, clearDirect: true, wantGrok: true, wantGPT: true},
		{name: "common group not bound to the key", exclusiveModels: []string{"gpt-5.6"}, commonModels: []string{"grok-4"}, wantGPT: true},
		{name: "key direct rule limits to codex", exclusiveModels: []string{"gpt-5.6"}, commonModels: []string{"grok-4"}, bindCommon: true, direct: &billing.RouteRule{CredentialIDs: []string{codex}}, wantGPT: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newConfiguredApp(t)
			const key = "dummy-grok-key"
			scope := billing.CallerScope(key)
			if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
				t.Fatal(err)
			}
			app.SetHostCaller(func(string, any) (json.RawMessage, error) {
				return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
					{ID: "dummy-codex", AuthIndex: "i-codex", Provider: "codex", Source: "file", Path: "/auth/codex.json", Email: "codex@example.test"},
					{ID: "dummy-xai", AuthIndex: "i-xai", Provider: "xai", Source: "file", Path: "/auth/xai.json", Email: "xai@example.test"},
				}}), nil
			})
			common := []string{}
			if tc.bindCommon {
				common = []string{scope}
			}
			callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Exclusive", "routing_mode": "exclusive", "scopes": []string{scope}, "rule": billing.RouteRule{Models: tc.exclusiveModels, CredentialIDs: []string{codex}}}, http.StatusCreated, nil)
			callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Common", "routing_mode": "common", "scopes": common, "rule": billing.RouteRule{Models: tc.commonModels, CredentialIDs: []string{xai}}}, http.StatusCreated, nil)
			if tc.direct != nil {
				if err := app.store.SetKeyRoutes(scope, billing.RouteBindings{Configured: true, RouteRule: *tc.direct}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.clearDirect {
				// The Clear action on the API key list sends an empty rule.
				callOK(t, app, http.MethodPut, routeKeysRoutes, nil, map[string]any{"scope": scope, "bindings": map[string]any{}}, http.StatusOK, nil)
			}
			if tc.protectCodex {
				if err := app.turnState.Update([]byte(`{"enabled":true,"dry_run":false,"probe_accounts":["dummy-codex"],"models":["gpt-5.6"]}`)); err != nil {
					t.Fatal(err)
				}
			}
			served := func(model, account, provider string) bool {
				raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
					RequestID: model, Model: model, SourceFormat: "openai",
					Metadata: map[string]any{MetadataCallerScope: scope},
				}))
				if err != nil {
					t.Fatal(err)
				}
				var before RequestInterceptResponse
				decodeResult(t, raw, &before)
				if before.Terminate {
					t.Logf("before-auth refused: %d %s", before.StatusCode, before.ResponseBody)
					return false
				}
				pick := schedulerRequest(scope, SchedulerAuthCandidate{ID: account, Provider: provider, Attributes: map[string]string{"path": "/auth/" + provider + ".json"}})
				pick.Model = model
				raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, pick))
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					OK     bool            `json:"ok"`
					Error  json.RawMessage `json:"error"`
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				if !envelope.OK {
					t.Logf("scheduler refused: %s", raw)
					return false
				}
				raw, err = app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
					RequestID: model, Model: model, SourceFormat: "openai",
					Metadata: map[string]any{MetadataCallerScope: scope, MetadataSelectedAuth: account},
				}))
				if err != nil {
					t.Fatal(err)
				}
				var after RequestInterceptResponse
				decodeResult(t, raw, &after)
				if after.Terminate {
					t.Logf("after-auth refused: %d %s", after.StatusCode, after.ResponseBody)
					return false
				}
				return true
			}
			if got := served("grok-4", "dummy-xai", "xai"); got != tc.wantGrok {
				t.Fatalf("grok served = %v, want %v", got, tc.wantGrok)
			}
			if tc.protectCodex {
				return // A protected account also needs a harvested template.
			}
			if got := served("gpt-5.6", "dummy-codex", "codex"); got != tc.wantGPT {
				t.Fatalf("gpt served = %v, want %v", got, tc.wantGPT)
			}
		})
	}
}
