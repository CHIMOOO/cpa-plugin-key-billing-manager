package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestPlanManagementScopeRoundTripAndStrictValidation(t *testing.T) {
	app := newAppWithPrice(t, true)
	response := app.createPlan(ManagementRequest{Body: []byte(`{"name":"Scoped","windows":[
{"name":"GPT 6","period_seconds":3600,"amount_usd":100,"scope":{"models":[" GPT-6-ASTRA "]}},
{"name":"Other OpenAI","period_seconds":3600,"amount_usd":200,"scope":{"providers":["openai"]}}
]}`)})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create scope response: %+v", response)
	}
	var body struct {
		Plan billing.Plan `json:"plan"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Plan.Windows) != 2 || body.Plan.Windows[0].Scope.Models[0] != "gpt-6-astra" {
		t.Fatalf("scope roundtrip: %+v", body)
	}
	for _, scope := range []string{`{"model":["gpt-5.5"]}`, `{"providers":["arbitrary"]}`, `{"models":["gpt-* "]}`, `{"models":["gpt-5.5"],"providers":["openai"]}`} {
		payload := `{"name":"Invalid","windows":[{"name":"Scope","period_seconds":3600,"amount_usd":1,"scope":` + scope + `}]}`
		response := app.createPlan(ManagementRequest{Body: []byte(payload)})
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			t.Fatalf("invalid scope response: %s %+v", scope, response)
		}
	}
	if len(app.store.Plans()) != 1 {
		t.Fatal("invalid scope persisted")
	}
}

func TestDynamicAutoQuotaRefusalDoesNotOpenCyclesOrLeakSlots(t *testing.T) {
	for _, format := range []string{"openai", "claude", "gemini"} {
		for _, source := range []string{"requested", "metadata", "model"} {
			t.Run(format+"/"+source, func(t *testing.T) {
				app := newAppWithPrice(t, true)
				if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
					t.Fatal(err)
				}
				if _, err := app.store.CreatePlanWithBindings(billing.Plan{Name: "Scoped", Windows: []billing.QuotaWindow{
					{Name: "GPT", PeriodSeconds: 3600, AmountUSD: 100, Scope: billing.QuotaScope{Models: []string{flowModel}}},
				}}, []string{flowScope()}); err != nil {
					t.Fatal(err)
				}
				request := RequestInterceptRequest{RequestID: "scoped-auto", SourceFormat: format, Model: flowModel, Metadata: flowMetadata()}
				switch source {
				case "requested":
					request.RequestedModel = "auto(high)"
				case "metadata":
					request.Metadata[MetadataRequestedModel] = "auto(high)"
				case "model":
					request.Model = "auto(high)"
				}
				raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, request))
				if err != nil {
					t.Fatal(err)
				}
				var response RequestInterceptResponse
				decodeResult(t, raw, &response)
				if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(response.ResponseBody), `"code":"quota_model_unresolved"`) || !strings.Contains(string(response.ResponseBody), "Choose an explicit model") {
					t.Fatalf("auto refusal = %+v body=%s", response, response.ResponseBody)
				}
				key, _ := app.store.KeyViewForScope(flowScope())
				if key.CurrentConcurrency != 0 || key.Windows[0].Started || key.Windows[0].Dimensions[0].Used != "0" {
					t.Fatalf("refusal modified quota/concurrency: %+v", key)
				}
				if events, err := app.store.RequestEvents(billing.RequestEventQuery{}); err != nil || events.Total != 0 {
					t.Fatalf("refusal billed: %+v %v", events, err)
				}
			})
		}
	}
}
