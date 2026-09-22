package plugin

import (
	"encoding/json"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestForceAstraUsesTargetStateTemplate(t *testing.T) {
	app := newConfiguredApp(t)
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "dummy-auth", AuthIndex: "dummy-index", Provider: "codex"},
		}}), nil
	})
	if err := app.turnState.Update([]byte(`{"force_astra":true,"enabled":true,"dry_run":false,"inject_mode":"always","probe_accounts":["dummy-auth"],"models":["gpt-6-astra","gpt-5.6-luna"]}`)); err != nil {
		t.Fatal(err)
	}
	// Learn a template under the routed model, then send a Luna request.
	turnStateAfterAuth(t, app, "learn-astra", "dummy-auth", "gpt-6-astra", nil)
	token := rpcTurnStateTemplate()
	if _, err := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, map[string]any{
		"RequestID": "learn-astra", "Model": "gpt-6-astra",
		"ResponseHeaders": map[string][]string{turnstate.Header: {token}},
	})); err != nil {
		t.Fatal(err)
	}
	raw, err := app.HandleMethod("model.route", mustMarshal(t, forceAstraRouteRequest{RequestedModel: "gpt-5.6-luna"}))
	if err != nil {
		t.Fatal(err)
	}
	var route forceAstraRouteResponse
	decodeResult(t, raw, &route)
	if !route.Handled || route.TargetModel != "gpt-6-astra" {
		t.Fatalf("missing Astra route: %+v", route)
	}
	got := turnStateAfterAuth(t, app, "routed-request", "dummy-auth", route.TargetModel, nil)
	if got.Terminate || got.Headers.Get(turnstate.Header) != token {
		t.Fatalf("routed request did not use Astra template: %+v", got)
	}
	if got := turnStateAfterAuth(t, app, "original-model", "dummy-auth", "gpt-5.6-luna", nil); got.Headers.Get(turnstate.Header) != "" {
		t.Fatal("Astra template leaked to original model")
	}
}

func TestForceAstraRouting(t *testing.T) {
	app := newConfiguredApp(t)
	if !registration().Capabilities.ModelRouter {
		t.Fatal("model router must be registered")
	}
	route := func(model string, metadata map[string]any) forceAstraRouteResponse {
		t.Helper()
		raw, err := app.HandleMethod("model.route", mustMarshal(t, forceAstraRouteRequest{RequestedModel: model, Metadata: metadata}))
		if err != nil {
			t.Fatal(err)
		}
		var result forceAstraRouteResponse
		decodeResult(t, raw, &result)
		return result
	}
	if route("gpt-5.6-luna", nil).Handled {
		t.Fatal("must default to off")
	}
	// Routing does not depend on State injection, harvesting, or dry-run mode.
	if err := app.turnState.Update([]byte(`{"force_astra":true,"enabled":false,"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-6-astra", "codex-mini-latest"} {
		got := route(model, nil)
		if !got.Handled || got.TargetKind != "provider" || got.Target != "codex" || got.TargetModel != "gpt-6-astra" {
			t.Fatalf("%s: %+v", model, got)
		}
	}
	if got := route("gpt-5.6-sol(high)", nil); got.TargetModel != "gpt-6-astra(high)" {
		t.Fatalf("reasoning suffix lost: %+v", got)
	}
	for _, model := range []string{"", "claude-sonnet-4-6", "gemini-3-pro", "my-alias", "vendor/gpt-5.6-sol", "gpt-image-1", "gpt-4o-audio-preview", "gpt-realtime", "gpt-4o-transcribe", "gpt-4o-mini-tts"} {
		if route(model, nil).Handled {
			t.Fatalf("unexpected override for %q", model)
		}
	}
	for _, meta := range []map[string]any{
		{MetadataRequestPath: "/v1/images/generations"},
		{MetadataRequestPath: "/v1/images/edits"},
		{MetadataSource: SourcePluginHostModelCallback},
	} {
		if route("gpt-5.6-sol", meta).Handled {
			t.Fatalf("unexpected override for %+v", meta)
		}
	}
	if err := app.turnState.Update([]byte(`{"force_astra":false}`)); err != nil {
		t.Fatal(err)
	}
	if route("gpt-5.6-luna", nil).Handled {
		t.Fatal("switch off must restore host routing")
	}
	if _, err := app.HandleMethod("model.route", []byte(`{`)); err == nil {
		t.Fatal("invalid route JSON accepted")
	}
}

func TestForceAstraPersists(t *testing.T) {
	path := t.TempDir() + "/billing.db"
	manager := turnstate.New()
	if err := manager.Configure(path); err != nil {
		t.Fatal(err)
	}
	if err := manager.Update([]byte(`{"force_astra":true}`)); err != nil {
		t.Fatal(err)
	}
	reloaded := turnstate.New()
	if err := reloaded.Configure(path); err != nil {
		t.Fatal(err)
	}
	if !reloaded.ForceAstraEnabled() || !reloaded.Status().Config.ForceAstra {
		t.Fatal("saved switch lost after reload")
	}
}
