package plugin

import (
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestLegacyForwardedForSettingDoesNotBlockRequests(t *testing.T) {
	app := newConfiguredApp(t)
	if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
		t.Fatal(err)
	}
	// Keep the old database setting readable, but never use it for admission.
	if _, err := app.store.SetForwardedForBlock(billing.ForwardedForBlock{
		Enabled: true, ModelKeywords: []string{"gpt"},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		RequestID: "legacy-forwarded-header", Model: "gpt-test",
		Headers:  http.Header{"X-Forwarded-For": {"203.0.113.7"}},
		Metadata: map[string]any{MetadataCallerScope: flowScope(), MetadataRequestPath: "/v1/responses"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	if response.Terminate {
		t.Fatalf("removed header rule refused request: %+v", response)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		response := callManagement(t, app, method, "/forwarded-for-block", nil, nil)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("removed endpoint %s status = %d", method, response.StatusCode)
		}
	}
}
