package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"cpa-key-billing/internal/billing"
)

func TestUIIncludesBothLanguagesWithoutExternalTranslationResources(t *testing.T) {
	for _, want := range []string{`<html lang="en">`, "const BILLING_MESSAGES =", "billing-language-change", `data-page-action`} {
		if !bytes.Contains(uiHTML, []byte(want)) {
			t.Fatalf("missing %q", want)
		}
	}
	if bytes.Contains(uiHTML, []byte("// BILLING_I18N")) {
		t.Fatal("unexpanded translation marker")
	}
}

func TestManagementValidationIncludesTranslationMetadata(t *testing.T) {
	err := (billing.Plan{}).Validate()
	if err == nil {
		t.Fatal("expected invalid plan")
	}
	response := errorResponse(err)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Key     string `json:"message_key"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || body.Error.Code != "invalid" || body.Error.Key == "" || body.Error.Message == "" {
		t.Fatalf("response = %s", response.Body)
	}
}

func TestGroupManagementMessagesPreserveUserLabels(t *testing.T) {
	app := newConfiguredApp(t)
	const label = "中文分组 {v0} 50%"
	if _, err := app.store.CreateRoute(billing.Route{ID: "group-i18n-route", Name: "Dummy route"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreateGroup(billing.KeyGroup{ID: "group-i18n", Name: label, RouteIDs: []string{"group-i18n-route"}}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := app.store.DeleteRoute("group-i18n-route")
	if err == nil {
		t.Fatal("a route referenced by a group must remain protected")
	}
	response := errorResponse(err)
	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Message string            `json:"message"`
			Key     string            `json:"message_key"`
			Params  map[string]string `json:"message_params"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict || body.Error.Code != "conflict" || body.Error.Key == "" || body.Error.Params["v0"] != `"`+label+`"` || !strings.Contains(body.Error.Message, label) {
		t.Fatalf("group validation lost translation metadata or changed user data: %s", response.Body)
	}
}

func TestProtocolRejectionsUseEnglishWithoutUITranslationMetadata(t *testing.T) {
	for _, format := range []string{"openai", "claude", "gemini"} {
		responses := []RequestInterceptResponse{
			concurrencyLimitResponse(format, billing.SlotDecision{Active: 2, Limit: 2}),
			quotaExhaustedResponse(format, billing.Decision{PlanName: "Test plan"}, time.Now()),
			modelForbiddenResponse(format, billing.RoutingDecision{Model: "test-model"}),
			routingConfigurationResponse(format, "Routing rule no longer exists"),
			priceRefusal(format, "model_price_error", "Model test-model has no configured price"),
		}
		for _, response := range responses {
			var payload struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(response.ResponseBody, &payload); err != nil {
				t.Fatal(err)
			}
			message, _ := payload.Error["message"].(string)
			if message == "" || strings.IndexFunc(message, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
				t.Fatalf("%s response must use English: %s", format, response.ResponseBody)
			}
			if payload.Error["message_key"] != nil || payload.Error["message_params"] != nil {
				t.Fatalf("UI metadata must not leak into the client protocol: %s", response.ResponseBody)
			}
		}
	}
	if strings.IndexFunc(MenuLabel+MenuDescription+noRoutedCredentialMessage, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
		t.Fatal("registration and scheduler diagnostics must use English")
	}
}
