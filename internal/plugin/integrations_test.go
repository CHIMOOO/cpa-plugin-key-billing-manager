package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

type integrationTestHost struct {
	Files     map[string]integrationAccount
	Requests  []hostHTTPRequest
	HTTP      func(hostHTTPRequest) (int, string)
	SaveError func(integrationAccount) error
}

func (h *integrationTestHost) call(method string, value any) (json.RawMessage, error) {
	encode := func(v any) (json.RawMessage, error) { return json.Marshal(v) }
	switch method {
	case hostAuthList:
		files := []hostAuthFile{}
		for name, account := range h.Files {
			files = append(files, hostAuthFile{ID: name, Name: name, AuthIndex: name, Type: account.Type, Disabled: true})
		}
		return encode(hostAuthListResponse{Files: files})
	case hostAuthGet:
		index := value.(map[string]any)["auth_index"].(string)
		account, exists := h.Files[index]
		if !exists {
			return nil, fmt.Errorf("not found")
		}
		raw, _ := json.Marshal(account)
		return encode(hostAuthGetResponse{JSON: raw})
	case "host.auth.save":
		input := value.(map[string]any)
		var account integrationAccount
		if err := json.Unmarshal(input["json"].(json.RawMessage), &account); err != nil {
			return nil, err
		}
		if h.SaveError != nil {
			if err := h.SaveError(account); err != nil {
				return nil, err
			}
		}
		h.Files[input["name"].(string)] = account
		return encode(map[string]any{"name": input["name"]})
	case hostHTTPDo:
		request := value.(hostHTTPRequest)
		h.Requests = append(h.Requests, request)
		if h.HTTP == nil {
			return nil, fmt.Errorf("no HTTP fixture")
		}
		status, body := h.HTTP(request)
		return encode(hostHTTPResponse{StatusCode: status, Body: []byte(body)})
	default:
		return nil, fmt.Errorf("unsupported host method")
	}
}

func integrationReq(t *testing.T, input any) ManagementRequest {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return ManagementRequest{Body: body, HostCallbackID: "dummy-callback"}
}

func TestIntegrationsUseHostSecretsAndPublishStableChannel(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{Files: map[string]integrationAccount{}, HTTP: func(req hostHTTPRequest) (int, string) {
		if req.URL != "https://opencode.ai/zen/v1/models" || req.Headers.Get("Authorization") != "Bearer dummy-integration-key" || req.HostCallbackID != "dummy-callback" {
			t.Fatalf("unexpected model request: %s", req.URL)
		}
		return 200, `{"data":[{"id":"glm-4.7"},{"id":"mimo-v2.5"}]}`
	}}
	app.SetHostCaller(host.call)
	response := app.saveIntegration(integrationReq(t, integrationInput{Name: "Zen account", Kind: "opencode-zen", APIKey: "dummy-integration-key"}))
	if response.StatusCode != 200 {
		t.Fatalf("save: %d %s", response.StatusCode, response.Body)
	}
	if response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("secret channel response may be cached")
	}
	var saved struct {
		Account integrationView `json:"account"`
		Channel map[string]any  `json:"channel"`
	}
	if err := json.Unmarshal(response.Body, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Channel["name"] != saved.Account.ChannelName || saved.Account.AuthFileName != integrationFileName(saved.Account.ID) {
		t.Fatal("channel identity differs from saved account")
	}
	if len(host.Files) != 1 || !host.Files[saved.Account.AuthFileName].Disabled || host.Files[saved.Account.AuthFileName].APIKey != "dummy-integration-key" {
		t.Fatal("credential did not reach host-owned storage")
	}
	list := app.listIntegrations(ManagementRequest{})
	if list.StatusCode != 200 || strings.Contains(string(list.Body), "dummy-integration-key") || !strings.Contains(string(list.Body), "Zen account") {
		t.Fatalf("unsafe list: %s", list.Body)
	}
	files := app.authFiles(viewAccess{})
	if strings.Contains(string(files.Body), saved.Account.AuthFileName) {
		t.Fatal("auxiliary credential leaked into routable OAuth list")
	}
	response = app.saveIntegration(integrationReq(t, integrationInput{ID: saved.Account.ID, Name: "Renamed", Kind: "opencode-zen"}))
	if response.StatusCode != 200 || host.Files[saved.Account.AuthFileName].APIKey != "dummy-integration-key" {
		t.Fatal("blank edit lost existing credential")
	}
	response = app.deleteIntegration(integrationReq(t, map[string]string{"id": saved.Account.ID}))
	if response.StatusCode != 200 {
		t.Fatalf("delete: %s", response.Body)
	}
	retired := host.Files[saved.Account.AuthFileName]
	if retired.APIKey != "" || retired.AuthCookie != "" || retired.RefreshToken != "" || !retired.Deleted {
		t.Fatal("retired credential retains secrets")
	}
	if strings.Contains(string(app.listIntegrations(ManagementRequest{}).Body), saved.Account.ID) {
		t.Fatal("deleted account remains visible")
	}
}

func TestIntegrationSecretsCannotBecomeURLsOrErrorMessages(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{Files: map[string]integrationAccount{}, HTTP: func(req hostHTTPRequest) (int, string) { return 401, `{"error":"dummy-secret echoed"}` }}
	app.SetHostCaller(host.call)
	for _, input := range []any{
		map[string]any{"name": "Bad", "kind": "opencode-go", "workspace_id": "../elsewhere", "auth_cookie": "dummy-cookie"},
		map[string]any{"name": "Bad", "kind": "opencode-zen", "api_key": "dummy\r\nHeader: injected"},
		map[string]any{"name": "Bad", "kind": "opencode-zen", "api_key": "dummy", "base_url": "https://example.invalid"},
	} {
		response := app.saveIntegration(integrationReq(t, input))
		if response.StatusCode != 400 {
			t.Fatalf("invalid input accepted: %d", response.StatusCode)
		}
	}
	response := app.saveIntegration(integrationReq(t, integrationInput{Name: "Secret", Kind: "opencode-zen", APIKey: "dummy-secret"}))
	if response.StatusCode != 502 || strings.Contains(string(response.Body), "dummy-secret") {
		t.Fatalf("unsafe upstream error: %s", response.Body)
	}
	if len(host.Files) != 0 {
		t.Fatal("failed catalog discovery persisted unusable account")
	}
	for _, endpoint := range resourceEndpoints {
		if strings.HasPrefix(endpoint.path, "/integrations") {
			t.Fatal("integration secrets exposed through resource routes")
		}
	}
}

func TestOpenCodeQuotaPreservesRealWindowsAndUnknownReset(t *testing.T) {
	now := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	body := `rollingUsage:$R[1]={usagePercent:12.5,resetInSec:90} weeklyUsage:$R[2]={resetInSec:600,usagePercent:81} monthlyUsage:$R[3]={usagePercent:101,resetInSec:1000}`
	rows := parseIntegrationOpenCodeQuota(body, now)
	if len(rows) != 3 || rows[0].WindowSeconds != 18000 || rows[1].WindowSeconds != 604800 || rows[0].Scope != "account" {
		t.Fatalf("windows: %+v", rows)
	}
	if *rows[0].RemainingPercent != 87.5 || rows[0].ResetAt != now.Add(90*time.Second).Format(time.RFC3339) || *rows[2].RemainingPercent != 0 {
		t.Fatalf("quota values: %+v", rows)
	}
	rows = parseIntegrationOpenCodeQuota(`<div data-slot="usage-item"><span data-slot="usage-label">Weekly</span><span data-slot="usage-value">25%</span></div>`, now)
	if len(rows) != 1 || *rows[0].RemainingPercent != 75 || rows[0].ResetAt != "" {
		t.Fatalf("HTML fallback fabricated reset: %+v", rows)
	}
}

func TestCommandCodeQueriesAuthenticatedOrganizationAndRealCaps(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{HTTP: func(req hostHTTPRequest) (int, string) {
		if req.Headers.Get("Authorization") != "Bearer dummy-command-key" {
			t.Fatal("wrong authorization")
		}
		switch req.URL {
		case "https://api.commandcode.ai/alpha/whoami?limits=1":
			return 200, `{"org":{"id":"org one"}}`
		case "https://api.commandcode.ai/alpha/billing/credits?orgId=org+one":
			return 200, `{"credits":{"monthlyCredits":40,"monthlyCreditsGranted":100,"purchasedCredits":5},"windowLimits":{"fiveHour":{"cap":20,"used":5,"resetAt":1800000000000},"weekly":{"cap":50,"used":25,"resetAt":1800100000000}}}`
		case "https://api.commandcode.ai/alpha/billing/subscriptions?orgId=org+one":
			return 200, `{"data":{"planId":"real-plan-id","status":"active","currentPeriodEnd":"2026-10-20T00:00:00Z"}}`
		default:
			t.Fatalf("unexpected endpoint: %s", req.URL)
			return 500, ""
		}
	}}
	app.SetHostCaller(host.call)
	result := authQuotaResponse{}
	if err := app.fetchCommandCodeQuota("dummy", integrationAccount{APIKey: "dummy-command-key"}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Quota) != 4 || result.Plan != "real-plan-id" || result.SubscriptionStatus != "active" || *result.Quota[0].RemainingPercent != 75 || result.Quota[0].ResetAt != time.UnixMilli(1800000000000).UTC().Format(time.RFC3339) {
		t.Fatalf("quota: %+v", result)
	}
	if result.Quota[3].Limit != nil || result.Quota[3].RemainingPercent != nil || *result.Quota[3].Remaining != 5 {
		t.Fatal("purchased credit balance was assigned an invented cap")
	}
}

func TestClineQuotaUnknownTypesStayUnknown(t *testing.T) {
	app := newConfiguredApp(t)
	host := &integrationTestHost{HTTP: func(req hostHTTPRequest) (int, string) {
		if req.URL != "https://api.cline.bot/api/v1/users/me/plan/usage-limits" || req.Headers.Get("Authorization") != "Bearer workos:dummy" {
			t.Fatal("wrong Cline quota request")
		}
		return 200, `{"success":true,"data":{"limits":[{"type":"five_hour","percentUsed":10,"resetsAt":"2026-09-20T02:00:00Z"},{"type":"future-bucket","percentUsed":20,"resetsAt":"2026-09-21T02:00:00Z"}]}}`
	}}
	app.SetHostCaller(host.call)
	result := authQuotaResponse{}
	if err := app.fetchClineSubscriptionQuota("dummy", integrationAccount{Kind: "cline-pass", APIKey: "workos:dummy"}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Quota) != 2 || result.Quota[0].WindowSeconds != 18000 || result.Quota[1].WindowSeconds != 0 || *result.Quota[1].RemainingPercent != 80 {
		t.Fatalf("quota: %+v", result)
	}
}

func TestClineDeviceFlowPollsOnceAndSavesRotatedTokensInHost(t *testing.T) {
	app := newConfiguredApp(t)
	authPolls := 0
	host := &integrationTestHost{Files: map[string]integrationAccount{}, HTTP: func(req hostHTTPRequest) (int, string) {
		switch req.URL {
		case "https://api.workos.com/user_management/authorize/device":
			return 200, `{"device_code":"dummy-device","user_code":"ABCD","verification_uri":"https://cline.bot/device","expires_in":600,"interval":5}`
		case "https://api.workos.com/user_management/authenticate":
			authPolls++
			if authPolls == 1 {
				return 400, `{"error":"slow_down"}`
			}
			return 200, `{"access_token":"dummy-access","refresh_token":"dummy-refresh"}`
		case "https://api.cline.bot/api/v1/auth/register":
			return 200, `{"success":true,"data":{"accessToken":"dummy-registered","refreshToken":"dummy-registered-refresh","expiresAt":"2026-12-20T00:00:00Z"}}`
		case "https://api.cline.bot/api/v1/models":
			return 200, `{"data":[{"id":"cline-pass/glm-5.3"},{"id":"payg/expensive-model"}]}`
		case "https://api.cline.bot/api/v1/auth/refresh":
			return 200, `{"data":{"accessToken":"dummy-new","refreshToken":"dummy-new-refresh"}}`
		default:
			t.Fatalf("unexpected endpoint %s", req.URL)
			return 500, ""
		}
	}}
	app.SetHostCaller(host.call)
	response := app.startIntegrationCline(integrationReq(t, map[string]string{"name": "Cline"}))
	var start struct {
		ID string `json:"login_id"`
	}
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &start) != nil || start.ID == "" {
		t.Fatalf("start: %s", response.Body)
	}
	pollReq := integrationReq(t, map[string]string{"login_id": start.ID})
	response = app.pollIntegrationCline(pollReq)
	if response.StatusCode != 200 || authPolls != 0 {
		t.Fatal("polled upstream before interval")
	}
	app.integrationLogins[start.ID].NextPoll = time.Time{}
	response = app.pollIntegrationCline(pollReq)
	if authPolls != 1 || app.integrationLogins[start.ID].Interval != 10 {
		t.Fatal("slow_down not respected")
	}
	app.integrationLogins[start.ID].NextPoll = time.Time{}
	response = app.pollIntegrationCline(pollReq)
	var complete struct {
		Status  string          `json:"status"`
		Account integrationView `json:"account"`
	}
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &complete) != nil || complete.Status != "complete" {
		t.Fatalf("complete: %s", response.Body)
	}
	stored := host.Files[complete.Account.AuthFileName]
	if stored.APIKey != "workos:dummy-registered" || stored.RefreshToken != "dummy-registered-refresh" || len(app.integrationLogins) != 0 {
		t.Fatal("tokens not persisted or session not released")
	}
	response = app.refreshIntegration(integrationReq(t, map[string]string{"id": complete.Account.ID}))
	stored = host.Files[complete.Account.AuthFileName]
	if response.StatusCode != 200 || stored.APIKey != "workos:dummy-new" || stored.RefreshToken != "dummy-new-refresh" || stored.ExpiresAt != "" {
		t.Fatalf("refresh: %s", response.Body)
	}
	if !strings.Contains(string(response.Body), "workos:dummy-new") || strings.Contains(string(response.Body), "dummy-new-refresh") {
		t.Fatal("channel cannot rotate or refresh secret leaked")
	}
}

func TestOpenCodePublishesOnlyVerifiedNativeProtocols(t *testing.T) {
	account := integrationAccount{ID: "aaaaaaaaaaaaaaaaaaaaaaaa", Kind: "opencode-zen", APIKey: "dummy-model-key", Models: []string{"glm-4.7", "gpt-5-nano", "claude-sonnet-4-6", "gemini-3.1-pro", "future-unknown"}}
	channels, unsupported := integrationChannels(account)
	if len(channels) != 3 || len(unsupported) != 2 {
		t.Fatalf("protocol plan: %+v %+v", channels, unsupported)
	}
	if channels[0].Models[0] != "glm-4.7" || channels[1].Models[0] != "gpt-5-nano" || channels[2].Models[0] != "claude-sonnet-4-6" {
		t.Fatal("native models were routed through the wrong protocol")
	}
	if channels[1].Config["base-url"] != "https://opencode.ai/zen/v1" || channels[2].Config["base-url"] != "https://opencode.ai/zen" {
		t.Fatal("host would append the wrong upstream endpoint")
	}
	if channels[2].Config["prefix"] == "" || channels[2].Config["models"].([]map[string]any)[0]["is-compat"] != true {
		t.Fatal("native external channel is not isolated or compatible")
	}
	other := account
	other.ID = "bbbbbbbbbbbbbbbbbbbbbbbb"
	otherChannels, _ := integrationChannels(other)
	if integrationExpectedCredentialID(account, channels[1]) == integrationExpectedCredentialID(other, otherChannels[1]) {
		t.Fatal("distinct integration accounts share a credential identity")
	}
	if integrationAccountHeaders(account).Get("X-Opencode-Session") == integrationAccountHeaders(other).Get("X-Opencode-Session") {
		t.Fatal("accounts share an OpenCode session")
	}
	app := newConfiguredApp(t)
	app.SetHostCaller((&integrationTestHost{Files: map[string]integrationAccount{}}).call)
	ref := billing.CredentialFingerprint(integrationExpectedCredentialID(account, channels[1]))
	app.credentials[ref] = credentialView{Ref: ref, Source: billing.CredentialSourceAIProviders, Provider: "claude"}
	if len(app.integrationView(account).CredentialRefs) != 0 {
		t.Fatal("wrong provider was accepted for an exact-looking ref")
	}
	app.credentials[ref] = credentialView{Ref: ref, Source: billing.CredentialSourceAIProviders, Provider: "codex"}
	view := app.integrationView(account)
	if len(view.CredentialRefs) != 1 || view.CredentialRefs[0] != ref || len(view.ClientModels) != 3 {
		t.Fatalf("synced config fallback: %+v", view)
	}
	account.Models = []string{"glm-4.7"}
	channels, _ = integrationChannels(account)
	if channels[1].Config != nil || channels[2].Config != nil {
		t.Fatal("removing a protocol model leaves its stale channel published")
	}
}
