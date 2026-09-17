package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestPriceAdmissionAndDeleteWithoutInventory(t *testing.T) {
	app := newConfiguredApp(t)
	intercept := func(format, model string) RequestInterceptResponse {
		t.Helper()
		raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{SourceFormat: format, Model: model, RequestedModel: model}))
		if err != nil {
			t.Fatal(err)
		}
		var result RequestInterceptResponse
		decodeResult(t, raw, &result)
		return result
	}
	for _, format := range []string{"openai", "openai-response", "claude", "gemini"} {
		result := intercept(format, "unpriced-dummy")
		if result.Terminate {
			t.Fatal("unpriced model was refused", format, result)
		}
	}
	for _, model := range []string{"unpriced-dummy", "gpt-4o"} {
		callOK(t, app, http.MethodPut, routePrices, nil, billing.CustomPrice{ModelID: model}, 200, nil)
		if result := intercept("openai", model); result.Terminate {
			t.Fatal("configured price rejected", result)
		}
		price, _, err := app.store.ResolveModelPrice(model, model, false)
		if err != nil || price.Source != billing.PriceSourceCustom || price.InputPer1M != 0 || price.OutputPer1M != 0 {
			t.Fatal(price, err)
		}
		callOK(t, app, http.MethodDelete, routePrices, url.Values{"model_id": {model}}, nil, 200, nil)
		if result := intercept("openai", model); result.Terminate {
			t.Fatal("deleting a price refused model access", result)
		}
	}
	callOK(t, app, http.MethodPut, routePrices, nil, billing.CustomPrice{ModelID: "gpt-*"}, 200, nil)
}

func TestBuiltinPriceAdmissionAndListing(t *testing.T) {
	app := newConfiguredApp(t)
	for _, test := range []struct {
		model      string
		input, out float64
	}{
		{model: "codex-auto-review", input: 2.5, out: 15},
		{model: "gpt-image-1.5", input: 5, out: 32},
	} {
		price, model, err := app.store.ResolveModelPrice(test.model, test.model, true)
		if err != nil || model != test.model || price.Source != billing.PriceSourceBuiltin || price.InputPer1M != test.input || price.OutputPer1M != test.out {
			t.Fatalf("builtin admission price = %+v, model=%q, error=%v", price, model, err)
		}
		rows := readPrices(t, app, test.model)
		if len(rows) != 1 || rows[0].Source != billing.PriceSourceBuiltin || rows[0].InputPer1M != test.input || rows[0].OutputPer1M != test.out {
			t.Fatalf("builtin listing = %+v", rows)
		}
	}
}

func TestUnpricedUsageStillEnforcesTokenAndRequestQuotas(t *testing.T) {
	for _, window := range []billing.QuotaWindow{
		{Name: "requests", RequestLimit: 1, PeriodSeconds: 3600},
		{Name: "tokens", TokenLimit: 125, PeriodSeconds: 3600},
	} {
		t.Run(window.Name, func(t *testing.T) {
			app := newConfiguredApp(t)
			const key = "sk-dummy-unpriced-quota-key"
			const model = "house-model-without-reference"
			scope := billing.CallerScope(key)
			if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
				t.Fatal(err)
			}
			if _, err := app.store.CreatePlanWithBindings(billing.Plan{
				ID: "unpriced-quota", Windows: []billing.QuotaWindow{window},
			}, []string{scope}); err != nil {
				t.Fatal(err)
			}
			request := RequestInterceptRequest{
				SourceFormat: "openai", Model: model, RequestedModel: model,
				Metadata: map[string]any{MetadataCallerScope: scope},
			}
			intercept := func() RequestInterceptResponse {
				t.Helper()
				raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, request))
				if err != nil {
					t.Fatal(err)
				}
				var response RequestInterceptResponse
				decodeResult(t, raw, &response)
				return response
			}
			if response := intercept(); response.Terminate {
				t.Fatalf("unpriced request was refused: %+v", response)
			}
			publishUsageRecord(t, app, UsageRecord{
				Model: model, Alias: model, APIKey: key, Provider: "openai", RequestedAt: app.store.Now(),
				Detail: UsageDetail{InputTokens: 100, OutputTokens: 25, TotalTokens: 125},
			})
			events := requestEventEntries(t, app)
			if len(events) != 1 || events[0].PriceSource != billing.PriceSourceNone ||
				events[0].Cost.TotalUSD != 0 || events[0].Cost.UncachedInputTokens != 100 || events[0].Cost.BilledOutputTokens != 25 {
				t.Fatalf("unpriced usage was not retained: %+v", events)
			}
			if response := intercept(); !response.Terminate || response.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("unpriced usage did not enforce %s quota: %+v", window.Name, response)
			}
		})
	}
}

func TestUsageAfterPriceDeletionKeepsTokensAndZeroCost(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			app := newConfiguredApp(t)
			model := "house-model-without-reference"
			_, err := app.store.UpsertPrice(billing.CustomPrice{
				ModelID:    model,
				PriceRates: billing.PriceRates{InputPer1M: 5, OutputPer1M: 10},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := RequestInterceptRequest{
				Model: model, RequestedModel: model, SourceFormat: "openai",
			}
			raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, request))
			if err != nil {
				t.Fatal(err)
			}
			var admission RequestInterceptResponse
			decodeResult(t, raw, &admission)
			if admission.Terminate {
				t.Fatal("priced request was refused")
			}
			if err := app.store.DeletePrice(model); err != nil {
				t.Fatal(err)
			}
			publishUsageRecord(t, app, UsageRecord{
				Model: model, Alias: model, APIKey: "sk-dummy-deleted-price",
				Provider: "openai", RequestedAt: app.store.Now(), Failed: failed,
				Detail: UsageDetail{InputTokens: 100, OutputTokens: 25, TotalTokens: 125},
			})
			events := requestEventEntries(t, app)
			if len(events) != 1 {
				t.Fatalf("usage event lost: %+v", events)
			}
			event := events[0]
			if event.Failed != failed || event.PriceSource != billing.PriceSourceNone ||
				event.Cost.TotalUSD != 0 || event.Cost.UncachedInputTokens != 100 ||
				event.Cost.BilledOutputTokens != 25 {
				t.Fatalf("unpriced usage was not retained at zero cost: %+v", event)
			}
		})
	}
}

func TestPriceListBatchesAndAccountCannotIncludeUnrequestedCustomPrices(t *testing.T) {
	app := newConfiguredApp(t)
	if _, err := app.store.UpsertPrice(billing.CustomPrice{ModelID: "retired"}); err != nil {
		t.Fatal(err)
	}
	var rows []billing.PriceRow
	query := url.Values{"model": {"gpt-4o"}, "include_custom": {"false"}}
	callOK(t, app, http.MethodGet, routePrices, query, nil, 200, &rows)
	if len(rows) != 1 || rows[0].Source != billing.PriceSourceReference || !rows[0].InModels {
		t.Fatalf("batch included unrelated custom prices: %+v", rows)
	}
	query.Set("include_custom", "true")
	callOK(t, app, http.MethodGet, routePrices, query, nil, 200, &rows)
	if len(rows) != 2 {
		t.Fatalf("admin union=%+v", rows)
	}
	response := callAccount(t, app, routePrices, accountTestKeyA, query)
	if err := json.Unmarshal(response.Body, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ModelID != "gpt-4o" {
		t.Fatalf("account expanded custom prices: %+v", rows)
	}
	query.Set("include_custom", "invalid")
	callOK(t, app, http.MethodGet, routePrices, query, nil, 400, nil)
}

func TestReferencePriceAdmissionAndUsageWithoutCustomPrices(t *testing.T) {
	app := newConfiguredApp(t)
	for i := range 2 {
		raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
			SourceFormat: "openai", Model: "gpt-4o", RequestedModel: "gpt-4o",
		}))
		if err != nil {
			t.Fatal(err)
		}
		var admission RequestInterceptResponse
		decodeResult(t, raw, &admission)
		if admission.Terminate {
			t.Fatalf("reference price rejected: %s", admission.ResponseBody)
		}
		publishUsageRecord(t, app, UsageRecord{
			Model: "gpt-4o", Alias: "gpt-4o", Provider: "openai", APIKey: "sk-dummy-reference",
			RequestedAt: app.store.Now(), Detail: UsageDetail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
		})
		entries := requestEventEntries(t, app)
		if len(entries) != i+1 || entries[0].PriceSource != billing.PriceSourceReference || entries[0].Cost.TotalUSD <= 0 {
			t.Fatalf("reference usage not billed: %+v", entries)
		}
	}
}

func TestCustomPriceIdentityAcrossListingAdmissionAndUsage(t *testing.T) {
	for _, test := range []struct {
		name       string
		requested  string
		customID   string
		billingID  string
		wantSource billing.PriceSource
		wantRate   float64
	}{
		{"thinking option", "gpt-4o(xhigh)", "gpt-4o", "gpt-4o", billing.PriceSourceCustom, 123},
		{"configured suffix", "gpt-4o(xhigh)", "gpt-4o(xhigh)", "gpt-4o(xhigh)", billing.PriceSourceCustom, 123},
		{"upstream custom price", "codex/gpt-4o", "gpt-4o", "codex/gpt-4o", billing.PriceSourceReference, 5},
		{"prefixed custom price", "codex/gpt-4o(xhigh)", "codex/gpt-4o", "codex/gpt-4o", billing.PriceSourceCustom, 123},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newConfiguredApp(t)
			if _, err := app.store.UpsertPrice(billing.CustomPrice{
				ModelID: test.customID, PriceRates: billing.PriceRates{InputPer1M: 123, OutputPer1M: 456},
			}); err != nil {
				t.Fatal(err)
			}
			var rows []billing.PriceRow
			callOK(t, app, http.MethodGet, routePrices,
				url.Values{"model": {test.requested}, "include_custom": {"false"}}, nil, 200, &rows)
			if len(rows) != 1 || rows[0].ModelID != test.requested || rows[0].Source != test.wantSource || rows[0].InputPer1M != test.wantRate {
				t.Fatalf("display price = %+v", rows)
			}
			if test.wantSource == billing.PriceSourceCustom && rows[0].CustomPriceModelID != test.customID {
				t.Fatalf("custom price mutation target = %q, want %q", rows[0].CustomPriceModelID, test.customID)
			}
			price, model, err := app.store.ResolveModelPrice(test.requested, test.requested, true)
			if err != nil || model != test.billingID || price.Source != test.wantSource || price.InputPer1M != test.wantRate {
				t.Fatalf("admission price = %+v, model = %q, error = %v", price, model, err)
			}
			publishUsageRecord(t, app, UsageRecord{
				Model: "gpt-4o", Alias: test.requested, Provider: "openai", APIKey: "sk-dummy-price-identity",
				RequestedAt: app.store.Now(), Detail: UsageDetail{InputTokens: 1000, TotalTokens: 1000},
			})
			events := requestEventEntries(t, app)
			if len(events) != 1 || events[0].BillingModel != test.billingID || events[0].PriceSource != test.wantSource || events[0].Cost.AppliedInputPer1M != test.wantRate {
				t.Fatalf("usage price = %+v", events)
			}
			assertCostClose(t, events[0].Cost.TotalUSD, test.wantRate/1000)
		})
	}
}

func TestReferencePriceSearchAcceptsCPAModelID(t *testing.T) {
	app := newConfiguredApp(t)
	var result struct {
		Prices []billing.ReferencePrice `json:"prices"`
	}
	callOK(t, app, http.MethodGet, routePrices+"/reference",
		url.Values{"q": {"codex/gpt-4o(xhigh)"}, "limit": {"1"}}, nil, 200, &result)
	if len(result.Prices) != 1 || result.Prices[0].ProviderID != "openai" || result.Prices[0].ModelID != "gpt-4o" || !result.Prices[0].IsCanonical {
		t.Fatalf("reference search = %+v", result.Prices)
	}
}
