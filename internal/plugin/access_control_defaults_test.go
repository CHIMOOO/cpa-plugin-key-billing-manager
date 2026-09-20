package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

// Exercise the actual SQLite-backed plugin and every access-control hook. A
// first request may reach the plugin before its key is synced from CPA.
func TestFreshInstallUngroupedPolicyAndSavedOptIn(t *testing.T) {
	config := testConfigYAML(t, true)
	open := func() *App {
		app := newTestApp(t)
		t.Cleanup(app.Shutdown)
		raw, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: config}))
		if err != nil {
			t.Fatal(err)
		}
		decodeResult(t, raw, nil)
		return app
	}
	app := open()
	if len(app.store.GroupViews()) != 0 {
		t.Fatal("fresh installation unexpectedly contains groups")
	}
	const ungroupedKey = "dummy-fresh-ungrouped-key"
	const groupedKey = "dummy-fresh-grouped-key"
	const allowedAuth = "dummy-fresh-allowed-auth"
	unknownScope := billing.CallerScope("dummy-not-yet-synced-key")
	ungroupedScope, groupedScope := billing.CallerScope(ungroupedKey), billing.CallerScope(groupedKey)
	models := []string{"gpt-6-astra", "gpt-5.6-sol", "claude-sonnet-4-5", "gemini-2.5-pro"}
	assertPolicy := func(deny bool) {
		t.Helper()
		var response struct {
			AccessControl billing.AccessControl `json:"access_control"`
		}
		callOK(t, app, http.MethodGet, routeAccessControl, nil, nil, http.StatusOK, &response)
		if !response.AccessControl.Enabled || response.AccessControl.DenyUngrouped != deny {
			t.Fatalf("management access-control response = %+v, want enabled with deny_ungrouped=%v", response, deny)
		}
		for _, scope := range []string{unknownScope, ungroupedScope} {
			for _, model := range models {
				assertUngroupedAccessHooks(t, app, scope, model, allowedAuth, deny)
			}
		}
	}
	assertPolicy(false)
	callOK(t, app, http.MethodPost, routeKeysSync, nil,
		map[string]any{"keys": []string{ungroupedKey, groupedKey}}, http.StatusOK, nil)
	assertPolicy(false)
	app.Shutdown()
	app = open()
	assertPolicy(false)

	// Creating an unrelated group does not automatically put ungrouped keys
	// under its credential restrictions or switch on the opt-in denial.
	if _, err := app.store.CreateGroup(billing.KeyGroup{Name: "Configured group", Rule: billing.RouteRule{
		CredentialIDs: []string{billing.CredentialFingerprint(allowedAuth)},
	}}, []string{groupedScope}); err != nil {
		t.Fatal(err)
	}
	assertPolicy(false)
	callOK(t, app, http.MethodPut, routeAccessControl, nil,
		map[string]bool{"enabled": true, "deny_ungrouped": true}, http.StatusOK, nil)
	assertPolicy(true)
	app.Shutdown()
	app = open()
	assertPolicy(true)
	for _, model := range models {
		assertUngroupedAccessHooks(t, app, groupedScope, model, allowedAuth, false)
	}

	// Turning it back off restores default access and survives another load.
	callOK(t, app, http.MethodPut, routeAccessControl, nil,
		map[string]bool{"enabled": true, "deny_ungrouped": false}, http.StatusOK, nil)
	assertPolicy(false)
	app.Shutdown()
	app = open()
	assertPolicy(false)
}

func assertUngroupedAccessHooks(t *testing.T, app *App, scope, model, auth string, denied bool) {
	t.Helper()
	req := RequestInterceptRequest{
		SourceFormat: "openai", Model: model, RequestedModel: model,
		Metadata: map[string]any{MetadataCallerScope: scope, MetadataSelectedAuth: auth},
	}
	for _, method := range []string{MethodRequestInterceptBefore, MethodRequestInterceptAfter} {
		raw, err := app.HandleMethod(method, mustMarshal(t, req))
		if err != nil {
			t.Fatal(err)
		}
		var response RequestInterceptResponse
		decodeResult(t, raw, &response)
		if response.Terminate != denied || denied && response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s scope=%q model=%q denied=%v: %+v", method, scope, model, denied, response)
		}
	}
	scheduling := schedulerRequest(scope, SchedulerAuthCandidate{ID: auth, Provider: "codex"})
	scheduling.Model = model
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, scheduling))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if denied {
		if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != http.StatusForbidden {
			t.Fatalf("scheduler scope=%q model=%q did not deny: %+v", scope, model, envelope)
		}
		return
	}
	var response SchedulerPickResponse
	decodeResult(t, raw, &response)
	if response.Handled {
		t.Fatalf("scheduler unexpectedly replaced CPA's unrestricted selection: %+v", response)
	}
}
