package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func learnTurnStateTemplate(t *testing.T, app *App, account, model string) string {
	t.Helper()
	id := "learn-" + account
	app.turnState.Before(id, account, model, nil)
	token := rpcTurnStateTemplate()
	if err := app.turnState.Learn(id, account, model, http.Header{turnstate.Header: {token}}); err != nil {
		t.Fatal(err)
	}
	app.turnState.Complete(id)
	return token
}

func schedulerPick(t *testing.T, app *App, req SchedulerPickRequest) (SchedulerPickResponse, bool) {
	t.Helper()
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var picked SchedulerPickResponse
	if envelope.OK {
		decodeResult(t, raw, &picked)
	}
	return picked, envelope.OK
}

// Once CPA has dropped the only account the key's groups allow, a selected
// State account with a valid template for the model serves the retry, but only
// while the breakout setting is on.
func TestBreakoutRetryLeavesTheGroupPoolForAReadyStateAccount(t *testing.T) {
	for _, breakout := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[breakout], func(t *testing.T) {
			app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-grouped")}})
			app.hostSchema.Store(6)
			config := map[string]any{"enabled": true, "dry_run": false, "inject_mode": "always", "breakout_retry": breakout,
				"probe_accounts": []string{"dummy-ready"}, "models": []string{"gpt-5.6"}}
			if err := app.turnState.Update(mustMarshal(t, config)); err != nil {
				t.Fatal(err)
			}
			token := learnTurnStateTemplate(t, app, "dummy-ready", "gpt-5.6")
			// The grouped account was already tried; CPA only offers the rest.
			picked, ok := schedulerPick(t, app, schedulerRequest(scope, SchedulerAuthCandidate{ID: "dummy-ready", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-outside", Provider: "codex"}))
			if ok != breakout || breakout && (!picked.Handled || picked.AuthID != "dummy-ready") {
				t.Fatalf("breakout=%v pick=%+v ok=%v", breakout, picked, ok)
			}
			raw, err := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
				RequestID: "retry", ToFormat: "codex", Model: "gpt-5.6", SourceFormat: "openai",
				Metadata: map[string]any{MetadataCallerScope: scope, MetadataSelectedAuth: "dummy-ready"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			var after RequestInterceptResponse
			decodeResult(t, raw, &after)
			if after.Terminate == breakout || breakout && after.Headers.Get(turnstate.Header) != token {
				t.Fatalf("breakout=%v after-auth=%+v", breakout, after)
			}
		})
	}
}

// Without the hard gate, the scheduler still avoids a selected account whose
// bucket expired while another candidate remains, and never strands a request.
func TestSchedulerAvoidsExpiredBucketsWhileAnotherAccountRemains(t *testing.T) {
	app := newConfiguredApp(t)
	app.setAccountRuntimeSettings(ManagementRequest{Body: []byte(`{"require_turn_state":false}`)})
	if err := app.turnState.Update([]byte(`{"enabled":true,"dry_run":false,"inject_mode":"always","probe_accounts":["dummy-stale","dummy-fresh"],"models":["gpt-5.6"]}`)); err != nil {
		t.Fatal(err)
	}
	learnTurnStateTemplate(t, app, "dummy-fresh", "gpt-5.6")
	both := schedulerRequest(flowScope(), SchedulerAuthCandidate{ID: "dummy-stale", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-fresh", Provider: "codex"})
	if picked, ok := schedulerPick(t, app, both); !ok || !picked.Handled || picked.AuthID != "dummy-fresh" {
		t.Fatalf("expired bucket was not avoided: %+v %v", picked, ok)
	}
	if picked, ok := schedulerPick(t, app, schedulerRequest(flowScope(), SchedulerAuthCandidate{ID: "dummy-stale", Provider: "codex"})); !ok || picked.Handled {
		t.Fatalf("the only remaining account was refused: %+v %v", picked, ok)
	}
	other := both
	other.Model = "gpt-other"
	if picked, ok := schedulerPick(t, app, other); !ok || picked.Handled {
		t.Fatalf("a model outside the State scope was steered: %+v %v", picked, ok)
	}
}

// The scheduler applies the same scope rule as the final check, and prefers
// accounts that check cannot refuse when the route model is only an alias.
func TestSchedulerProtectsOnlySelectedModels(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.turnState.Update([]byte(`{"enabled":true,"dry_run":false,"inject_mode":"always","probe_accounts":["dummy-a"],"models":["gpt-6-astra"]}`)); err != nil {
		t.Fatal(err)
	}
	pick := func(model string, candidates ...SchedulerAuthCandidate) (SchedulerPickResponse, bool) {
		t.Helper()
		req := schedulerRequest(flowScope(), candidates...)
		req.Model = model
		return schedulerPick(t, app, req)
	}
	selected := SchedulerAuthCandidate{ID: "dummy-a", Provider: "codex"}
	plain := SchedulerAuthCandidate{ID: "dummy-plain", Provider: "codex"}
	app.hostSchema.Store(5)
	if _, ok := pick("gpt-other", selected); !ok {
		t.Fatal("an out-of-scope model was refused on an old host")
	}
	if _, ok := pick("GPT-6-Astra(x)", selected); ok {
		t.Fatal("an odd spelling of a selected model escaped the old-host refusal")
	}
	app.hostSchema.Store(6)
	if picked, ok := pick("my-astra", selected, plain); !ok || !picked.Handled || picked.AuthID != "dummy-plain" {
		t.Fatalf("alias did not prefer an account the final check accepts: %+v %v", picked, ok)
	}
	if picked, ok := pick("my-astra", selected); !ok || picked.Handled {
		t.Fatalf("the only account was refused for an alias: %+v %v", picked, ok)
	}
}
