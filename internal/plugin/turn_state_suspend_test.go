package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/turnstate"
)

func TestTurnStateSuspendBypassesEveryPathAndKeepsSettings(t *testing.T) {
	configYAML := testConfigYAML(t, true)
	register := func(app *App) Registration {
		t.Helper()
		raw, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: configYAML}))
		if err != nil {
			t.Fatal(err)
		}
		var reg Registration
		decodeResult(t, raw, &reg)
		return reg
	}
	app := newTestApp(t)
	t.Cleanup(app.Shutdown)
	if !register(app).Capabilities.StreamChunkInterceptor {
		t.Fatal("active State must register its hooks")
	}
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{
			{ID: "dummy-auth", AuthIndex: "dummy-index", Provider: "codex", Type: "codex", Source: "file"},
		}}), nil
	})
	// Injection off plus a protected account normally blocks every request.
	if err := app.turnState.Update([]byte(`{"force_astra":true,"enabled":false,"inject_mode":"always","probe_accounts":["dummy-auth"]}`)); err != nil {
		t.Fatal(err)
	}
	if got := turnStateAfterAuth(t, app, "gated", "dummy-auth", "gpt-6-astra", nil); !got.Terminate {
		t.Fatal("baseline: protected account must be gated")
	}

	if err := app.turnState.Update([]byte(`{"suspended":true}`)); err != nil {
		t.Fatal(err)
	}
	cfg := app.turnState.Status().Config
	if !cfg.Suspended || !cfg.ForceAstra || cfg.InjectMode != "always" || len(cfg.ProbeAccounts) != 1 {
		t.Fatalf("suspending must keep other settings: %+v", cfg)
	}
	if got := turnStateAfterAuth(t, app, "passthrough", "dummy-auth", "gpt-6-astra", nil); got.Terminate || got.Headers != nil || got.ClearHeaders != nil {
		t.Fatalf("suspended State must pass requests through unchanged: %+v", got)
	}
	raw, err := app.HandleMethod("model.route", mustMarshal(t, forceAstraRouteRequest{RequestedModel: "gpt-5.6-luna"}))
	if err != nil {
		t.Fatal(err)
	}
	var route forceAstraRouteResponse
	decodeResult(t, raw, &route)
	if route.Handled {
		t.Fatalf("suspended State must not reroute: %+v", route)
	}
	// Response learning must not fill a bucket while suspended.
	if err := app.turnState.Update([]byte(`{"enabled":true,"dry_run":false}`)); err != nil {
		t.Fatal(err)
	}
	turnStateAfterAuth(t, app, "learn", "dummy-auth", "gpt-6-astra", nil)
	if _, err := app.HandleMethod(MethodResponseInterceptAfter, mustMarshal(t, map[string]any{
		"RequestID": "learn", "Model": "gpt-6-astra",
		"ResponseHeaders": map[string][]string{turnstate.Header: {rpcTurnStateTemplate()}},
	})); err != nil {
		t.Fatal(err)
	}
	if n := len(app.turnState.Status().Templates); n != 0 {
		t.Fatalf("suspended State learned %d templates", n)
	}
	if got := app.probeTurnState(ManagementRequest{Body: []byte(`{}`)}); got.StatusCode != http.StatusConflict {
		t.Fatalf("suspended State must refuse probes: %d", got.StatusCode)
	}
	if got := app.setTurnStateRunner(ManagementRequest{Body: []byte(`{"enabled":true}`)}); got.StatusCode != http.StatusConflict {
		t.Fatalf("suspended State must refuse starting collection: %d", got.StatusCode)
	}

	// A restarted CPA loads the persisted switch and omits State-only hooks.
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	caps := register(restarted).Capabilities
	if !restarted.turnState.Status().Config.Suspended {
		t.Fatal("the global switch must survive a restart")
	}
	if caps.ModelRouter || caps.ResponseInterceptor || caps.StreamChunkInterceptor || !caps.RequestInterceptor || !caps.Scheduler || !caps.UsagePlugin {
		t.Fatalf("unexpected capabilities while suspended: %+v", caps)
	}

	if err := app.turnState.Update([]byte(`{"suspended":false}`)); err != nil {
		t.Fatal(err)
	}
	if got := turnStateAfterAuth(t, app, "resumed", "dummy-auth", "gpt-6-astra", nil); !got.Terminate {
		t.Fatal("resuming must restore the protected-account gate")
	}
	if !registrationForHost(0, app.turnState.Active(), app.turnState.Active()).Capabilities.StreamChunkInterceptor {
		t.Fatal("resumed State must register its hooks again")
	}
}
