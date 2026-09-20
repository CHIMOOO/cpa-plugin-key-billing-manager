package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func enableImageTestState(t *testing.T, app *App) {
	t.Helper()
	app.hostSchema.Store(6)
	if err := app.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["dummy-image-account"],"models":["dummy-chat-model"]}`)); err != nil {
		t.Fatal(err)
	}
}

func imageTestRequest(id, path string) RequestInterceptRequest {
	return RequestInterceptRequest{RequestID: id, SourceFormat: "openai", ToFormat: "codex", Model: "dummy-image-model",
		Metadata: map[string]any{MetadataCallerScope: flowScope(), MetadataSelectedAuth: "dummy-image-account", MetadataRequestPath: path}}
}

func imageTestIntercept(t *testing.T, app *App, method string, req RequestInterceptRequest) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(method, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var result RequestInterceptResponse
	decodeResult(t, raw, &result)
	return result
}

func TestImageEndpointsDoNotRequireChatState(t *testing.T) {
	app := newConfiguredApp(t)
	enableImageTestState(t, app)
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		t.Run(path, func(t *testing.T) {
			req := imageTestRequest(path, path)
			// These model/body hints are deliberately indistinguishable from a
			// chat call. The host's explicit endpoint is the only exemption.
			req.Model = "dummy-chat-model"
			req.Body = []byte("unchanged multipart image upload")
			result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req)
			if result.Terminate || len(result.Headers) != 0 || len(result.ClearHeaders) != 0 {
				t.Fatalf("image call was modified or refused by State: %+v", result)
			}
			scheduling := schedulerRequest(flowScope(), SchedulerAuthCandidate{ID: "dummy-image-account", Provider: "codex"})
			scheduling.Model, scheduling.Options.Metadata[MetadataRequestPath] = req.Model, path
			raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, scheduling))
			if err != nil {
				t.Fatal(err)
			}
			var picked SchedulerPickResponse
			decodeResult(t, raw, &picked)
			if picked.Handled {
				t.Fatal("image scheduler unexpectedly replaced the host's unchanged pool")
			}
		})
	}
	for _, path := range []string{"", "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/images/generations", "/v1/images/unknown", "/v1/images/generations/"} {
		t.Run("protected-"+path, func(t *testing.T) {
			req := imageTestRequest("not-image", path)
			req.Body = []byte(`{"tools":[{"type":"image_generation"}]}`)
			result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req)
			if !result.Terminate || !strings.Contains(string(result.ResponseBody), "turn_state_required") {
				t.Fatalf("unknown/chat endpoint escaped State: %+v", result)
			}
		})
	}
}

func TestImageStateExemptionKeepsCredentialRoutingAndDisabledFiltering(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{
		billing.CredentialFingerprint("dummy-image-account"), billing.CredentialFingerprint("dummy-disabled-account"),
	}})
	enableImageTestState(t, app)
	scheduling := schedulerRequest(scope,
		SchedulerAuthCandidate{ID: "dummy-image-account", Provider: "codex"},
		SchedulerAuthCandidate{ID: "dummy-disabled-account", Provider: "codex", Status: "disabled"},
		SchedulerAuthCandidate{ID: "dummy-outside-group", Provider: "codex"})
	scheduling.Options.Metadata[MetadataRequestPath] = "/v1/images/generations"
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, scheduling))
	if err != nil {
		t.Fatal(err)
	}
	var picked SchedulerPickResponse
	decodeResult(t, raw, &picked)
	if !picked.Handled || picked.AuthID != "dummy-image-account" {
		t.Fatalf("State exemption expanded the credential pool: %+v", picked)
	}
	scheduling.Options.Metadata[MetadataRequestPath] = "/v1/responses"
	raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, scheduling))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.OK {
		t.Fatalf("chat scheduler stopped enforcing missing State: %s (%v)", raw, err)
	}
	for _, account := range []string{"", "dummy-outside-group"} {
		req := imageTestRequest("denied", "/v1/images/edits")
		req.Metadata[MetadataCallerScope], req.Metadata[MetadataSelectedAuth] = scope, account
		result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req)
		if !result.Terminate || result.StatusCode != http.StatusForbidden {
			t.Fatalf("unverified/disallowed image credential accepted: %+v", result)
		}
	}
}

func TestImageStateExemptionKeepsQuotasAndConcurrency(t *testing.T) {
	t.Run("model-policy", func(t *testing.T) {
		app, scope := configuredRoutingApp(t, billing.RouteRule{Models: []string{"dummy-chat-model"}})
		enableImageTestState(t, app)
		req := imageTestRequest("forbidden-model", "/v1/images/generations")
		req.Metadata[MetadataCallerScope] = scope
		result := imageTestIntercept(t, app, MethodRequestInterceptBefore, req)
		if !result.Terminate || result.StatusCode != http.StatusForbidden {
			t.Fatalf("image request bypassed model policy: %+v", result)
		}
	})
	t.Run("quota", func(t *testing.T) {
		app := exhaustedApp(t, 30*time.Minute)
		enableImageTestState(t, app)
		req := imageTestRequest("quota", "/v1/images/generations")
		req.Model = "gpt-5.5"
		result := imageTestIntercept(t, app, MethodRequestInterceptBefore, req)
		if !result.Terminate || result.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("image request bypassed quota: %+v", result)
		}
	})
	t.Run("key-concurrency", func(t *testing.T) {
		app := newConfiguredApp(t)
		enableImageTestState(t, app)
		if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
			t.Fatal(err)
		}
		if err := app.store.SetConcurrencyLimit(flowScope(), 1); err != nil {
			t.Fatal(err)
		}
		first := imageTestRequest("key-slot-1", "/v1/images/generations")
		if result := imageTestIntercept(t, app, MethodRequestInterceptBefore, first); result.Terminate {
			t.Fatal(result)
		}
		second := imageTestRequest("key-slot-2", "/v1/images/edits")
		if result := imageTestIntercept(t, app, MethodRequestInterceptBefore, second); !result.Terminate || result.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("image request bypassed API key concurrency: %+v", result)
		}
		if _, err := app.completeRequest(mustMarshal(t, RequestCompletion{RequestID: first.RequestID})); err != nil {
			t.Fatal(err)
		}
		if result := imageTestIntercept(t, app, MethodRequestInterceptBefore, second); result.Terminate {
			t.Fatalf("image completion leaked API key slot: %+v", result)
		}
	})
	t.Run("account-concurrency", func(t *testing.T) {
		app := newConfiguredApp(t)
		enableImageTestState(t, app)
		settings := accountRuntimeSettings{RequireTurnState: true, Accounts: map[string]accountRuntimePolicy{
			billing.CredentialFingerprint("dummy-image-account"): {ConcurrencyLimit: 1},
		}}
		if result := app.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, settings)}); result.StatusCode != 200 {
			t.Fatal(string(result.Body))
		}
		first := imageTestRequest("account-slot-1", "/v1/images/generations")
		if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, first); result.Terminate {
			t.Fatal(result)
		}
		second := imageTestRequest("account-slot-2", "/v1/images/edits")
		if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, second); !result.Terminate || result.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("image request bypassed account concurrency: %+v", result)
		}
		second.Metadata[MetadataSelectedAuth] = ""
		if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, second); !result.Terminate || !strings.Contains(string(result.ResponseBody), "account_identity_required") {
			t.Fatalf("image request escaped account identity enforcement: %+v", result)
		}
		if _, err := app.completeRequest(mustMarshal(t, RequestCompletion{RequestID: first.RequestID})); err != nil {
			t.Fatal(err)
		}
		second.Metadata[MetadataSelectedAuth] = "dummy-image-account"
		if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, second); result.Terminate {
			t.Fatalf("image completion leaked account slot: %+v", result)
		}
	})
}

func TestImageResponsesCannotLearnChatStateAcrossRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, responseHasPath := range []bool{false, true} {
			app := newConfiguredApp(t)
			enableImageTestState(t, app)
			// Simulate a previous chat attempt sharing this request ID. An image
			// retry must clear it even when the response hook lacks metadata.
			app.turnState.Before("retry", "dummy-image-account", "dummy-chat-model", nil)
			req := imageTestRequest("retry", "/v1/images/edits")
			if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req); result.Terminate {
				t.Fatal(result)
			}
			response := map[string]any{"RequestID": req.RequestID, "Model": req.Model, "ChunkIndex": -1,
				"ResponseHeaders": http.Header{turnstate.Header: {rpcTurnStateTemplate()}}, "Body": []byte("original image bytes")}
			if responseHasPath {
				response["Metadata"] = req.Metadata
			}
			raw, err := app.handleTurnStateResponse(mustMarshal(t, response), stream)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			decodeResult(t, raw, &result)
			if len(result) != 0 || len(app.turnState.Status().Templates) != 0 {
				t.Fatalf("image response changed bytes or learned a chat template: %s", raw)
			}
		}
	}
}

func TestImagesNeverReplayExistingChatTemplate(t *testing.T) {
	app := newConfiguredApp(t)
	enableImageTestState(t, app)
	token := rpcTurnStateTemplate()
	app.turnState.Before("seed", "dummy-image-account", "dummy-chat-model", nil)
	if err := app.turnState.Learn("seed", "dummy-image-account", "dummy-chat-model", http.Header{turnstate.Header: {token}}); err != nil {
		t.Fatal(err)
	}
	req := imageTestRequest("existing-template", "/v1/images/generations")
	req.Model = "dummy-chat-model"
	if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req); result.Terminate || len(result.Headers) != 0 || len(result.ClearHeaders) != 0 {
		t.Fatalf("chat template was injected on an image endpoint: %+v", result)
	}
	req.RequestID, req.Metadata[MetadataRequestPath] = "chat", "/v1/responses"
	if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req); result.Terminate || result.Headers.Get(turnstate.Header) != token {
		t.Fatalf("image exemption changed normal chat injection: %+v", result)
	}
}

func TestImageRejectedRetryAndIntermediateChunkClearChatAttribution(t *testing.T) {
	for _, phase := range []string{"rejected-retry", "intermediate-chunk", "completion"} {
		t.Run(phase, func(t *testing.T) {
			app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-other-account")}})
			enableImageTestState(t, app)
			app.turnState.Before("stale", "dummy-image-account", "dummy-chat-model", nil)
			switch phase {
			case "rejected-retry":
				req := imageTestRequest("stale", "/v1/images/edits")
				req.Metadata[MetadataCallerScope] = scope
				if result := imageTestIntercept(t, app, MethodRequestInterceptAfter, req); !result.Terminate || result.StatusCode != http.StatusForbidden {
					t.Fatalf("test expected credential rejection: %+v", result)
				}
			case "intermediate-chunk":
				_, err := app.handleTurnStateResponse(mustMarshal(t, map[string]any{
					"RequestID": "stale", "ChunkIndex": 0, "Metadata": map[string]any{MetadataRequestPath: "/v1/images/generations"},
				}), true)
				if err != nil {
					t.Fatal(err)
				}
			case "completion":
				if _, err := app.completeRequest(mustMarshal(t, RequestCompletion{RequestID: "stale"})); err != nil {
					t.Fatal(err)
				}
			}
			// A later response with no path metadata must not recover the old
			// chat attribution after an image/rejected/completed request.
			if _, err := app.handleTurnStateResponse(mustMarshal(t, map[string]any{
				"RequestID": "stale", "Model": "dummy-chat-model", "ResponseHeaders": http.Header{turnstate.Header: {rpcTurnStateTemplate()}},
			}), false); err != nil {
				t.Fatal(err)
			}
			if len(app.turnState.Status().Templates) != 0 {
				t.Fatal("stale image request attribution learned a chat template")
			}
		})
	}
}
