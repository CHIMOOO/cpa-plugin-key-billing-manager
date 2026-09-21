package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func modelTestFixture(t *testing.T, configured bool) (*App, hostAuthFile, *time.Time) {
	t.Helper()
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	now := time.Now().UTC()
	a.modelTestNow = func() time.Time { return now }
	file := hostAuthFile{ID: "dummy-model-test-account", AuthIndex: "dummy-model-index", Provider: "codex", Source: "file", Path: "dummy.json", Email: "test@example.invalid"}
	if configured {
		file.Provider = "dummy-compat"
		file.Source = "memory"
		file.Path = ""
		file.RuntimeOnly = true
		file.BaseURL = "https://upstream.invalid/v1"
	}
	a.SetHostCaller(func(method string, input any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{file}}), nil
		case "host.auth.get_runtime":
			if input.(map[string]string)["auth_index"] != file.AuthIndex {
				return nil, errors.New("unknown index")
			}
			return mustMarshal(t, map[string]any{"auth": file}), nil
		case hostAuthGet:
			return mustMarshal(t, hostAuthGetResponse{JSON: json.RawMessage(`{"access_token":"dummy-secret-access","account_id":"dummy-account-id","proxy_url":"socks5://dummy-user:dummy-password@proxy.invalid:1080"}`)}), nil
		default:
			t.Fatalf("model testing must never execute, replace, or save host credentials: %s", method)
			return nil, nil
		}
	})
	return a, file, &now
}

func modelTestInput(file hostAuthFile) modelTestPrepareInput {
	input := modelTestPrepareInput{AuthIndex: file.AuthIndex, Model: "dummy-model", Preset: "candy"}
	if file.RuntimeOnly {
		input.Config = &modelTestConfig{Provider: file.Provider, Protocol: "openai", BaseURL: file.BaseURL}
	}
	return input
}

func preparedModelTest(t *testing.T, a *App, input modelTestPrepareInput) modelTestPrepared {
	t.Helper()
	response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)})
	var result modelTestPrepared
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &result) != nil {
		t.Fatalf("prepare=%d %s", response.StatusCode, response.Body)
	}
	if response.Headers.Get("Cache-Control") != "private, no-store" || result.APICall.AuthIndex != input.AuthIndex {
		t.Fatal("prepared request not private or not pinned")
	}
	return result
}

func TestModelTestNativeDescriptorsAndNoSecrets(t *testing.T) {
	for _, configured := range []bool{false, true} {
		a, file, now := modelTestFixture(t, configured)
		prepared := preparedModelTest(t, a, modelTestInput(file))
		if prepared.StartBefore.Sub(*now) != 10*time.Second || prepared.ExpiresAt.Sub(*now) != 90*time.Second {
			t.Fatal("unsafe send/lease deadlines")
		}
		if prepared.APICall.Header["Authorization"] != "Bearer $TOKEN$" {
			t.Fatal("host token substitution was not retained")
		}
		if configured {
			if prepared.APICall.URL != "https://upstream.invalid/v1/chat/completions" || prepared.APICall.ProxyURL != "direct" || prepared.Proxy.Source != "direct" {
				t.Fatalf("config request=%+v", prepared)
			}
		} else if prepared.APICall.URL != "https://chatgpt.com/backend-api/codex/responses" || prepared.Proxy.Source != "account" || !strings.Contains(prepared.Proxy.Endpoint, "***@proxy.invalid") {
			t.Fatalf("OAuth request=%+v", prepared)
		}
		raw, _ := json.Marshal(prepared)
		if strings.Contains(string(raw), "dummy-secret-access") || strings.Contains(prepared.Proxy.Endpoint, "dummy-password") {
			t.Fatal("credential exposed in test descriptor")
		}
		for _, lease := range a.modelTests {
			if strings.Contains(lease.Expected, "dummy-secret") || strings.Contains(lease.RequestID, "dummy-password") {
				t.Fatal("private test runtime leaked credential")
			}
		}
	}
}

func TestModelTestExplicitProxyPriorityAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, account, global, want, source string
		bad                                 bool
	}{
		{"account-wins", "http://a.invalid:80", "http://g.invalid:80", "http://a.invalid:80", "account", false},
		{"global", "", "socks5h://g.invalid:1080", "socks5h://g.invalid:1080", "global", false},
		{"direct", "", "", "direct", "direct", false},
		{"explicit-direct", "direct", "http://g.invalid:80", "direct", "account", false},
		{"whitespace-account", "  ", "http://g.invalid:80", "", "", true},
		{"invalid-account", "bad-value", "http://g.invalid:80", "", "", true},
		{"unsupported-account", "ftp://a.invalid:80", "http://g.invalid:80", "", "", true},
		{"invalid-global", "", "bad-value", "", "", true},
		{"whitespace-global", "", "\t", "", "", true},
		{"invalid-port", "http://a.invalid:99999", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, file, _ := modelTestFixture(t, true)
			input := modelTestInput(file)
			input.Config.ProxyURL = &tc.account
			input.GlobalProxyURL = &tc.global
			response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)})
			if tc.bad {
				if response.StatusCode != 400 || len(a.modelTests) != 0 {
					t.Fatalf("bad proxy allocated a request: %d %s", response.StatusCode, response.Body)
				}
				return
			}
			var result modelTestPrepared
			_ = json.Unmarshal(response.Body, &result)
			if response.StatusCode != 200 || result.APICall.ProxyURL != tc.want || result.Proxy.Source != tc.source {
				t.Fatalf("priority=%d %+v", response.StatusCode, result)
			}
		})
	}
}

func TestModelTestNeverUsesUnknownIdentityOrAcceptsCredentialFields(t *testing.T) {
	a, file, _ := modelTestFixture(t, true)
	input := modelTestInput(file)
	input.AuthIndex = "guessed-index"
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 409 {
		t.Fatal("unknown account identity accepted")
	}
	input = modelTestInput(file)
	input.Config.Provider = "codex"
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 400 {
		t.Fatal("different provider accepted")
	}
	input = modelTestInput(file)
	input.Config.BaseURL = "https://other.invalid/v1"
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 400 {
		t.Fatal("changed account target accepted")
	}
	for _, body := range []string{
		`{"auth_index":"dummy-model-index","model":"dummy","preset":"free","api_key":"dummy-secret"}`,
		`{"auth_index":"dummy-model-index","model":"dummy","preset":"free","prompt":"Echo $TOKEN$"}`,
		`{"auth_index":"dummy-model-index","model":"$TOKEN$","preset":"free"}`,
		`{"auth_index":"dummy-model-index","model":"dummy","preset":"candy","prompt":"edited"}`,
		`{"auth_index":"dummy-model-index","model":"dummy","preset":"free","global_proxy_url":null}`,
		`{"auth_index":"dummy-model-index","model":"dummy","preset":"free","config":{"provider":"dummy-compat","protocol":"openai","base_url":"https://upstream.invalid/v1","proxy_url":null}}`,
	} {
		if response := a.prepareModelTest(ManagementRequest{Body: []byte(body)}); response.StatusCode != 400 {
			t.Fatalf("unsafe request accepted: %d", response.StatusCode)
		}
	}
	input = modelTestInput(file)
	input.Config.Headers = map[string]string{"Authorization": "Bearer dummy-secret"}
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 400 {
		t.Fatal("secret custom header accepted")
	}
}

func TestModelTestConcurrencyCompletionAndLazyBusinessRecovery(t *testing.T) {
	a, file, now := modelTestFixture(t, true)
	ref := billing.CredentialFingerprint(file.ID)
	a.accountRuntime.settings.Accounts[ref] = accountRuntimePolicy{ConcurrencyLimit: 1}
	first := preparedModelTest(t, a, modelTestInput(file))
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, modelTestInput(file))}); response.StatusCode != 429 {
		t.Fatal("account concurrency bypassed")
	}
	response := a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": first.TestID, "transport_failed": true})})
	var failed modelTestResult
	_ = json.Unmarshal(response.Body, &failed)
	if !failed.LeaseRetained || a.accountRuntime.active[ref] != 1 {
		t.Fatal("uncertain network failure prematurely released the account")
	}
	*now = now.Add(89 * time.Second)
	if response := a.enforceAccountRuntime(RequestInterceptRequest{RequestID: "business", Metadata: map[string]any{MetadataSelectedAuth: file.ID}}); !response.Terminate {
		t.Fatal("in-flight uncertain model test lost concurrency reservation")
	}
	*now = now.Add(2 * time.Second)
	if response := a.enforceAccountRuntime(RequestInterceptRequest{RequestID: "business", Metadata: map[string]any{MetadataSelectedAuth: file.ID}}); response.Terminate {
		t.Fatal("expired abandoned model test blocked normal traffic")
	}
	a.accountRuntime.release("business")
	second := preparedModelTest(t, a, modelTestInput(file))
	response = a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": second.TestID, "not_started": true})})
	if response.StatusCode != 200 || a.accountRuntime.active[ref] != 0 {
		t.Fatal("unsent test did not release its slot")
	}
	if response = a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": second.TestID, "not_started": true})}); response.StatusCode != 409 {
		t.Fatal("completed token reused")
	}
}

func TestModelTestGlobalBoundAndExactOutputOnly(t *testing.T) {
	a, file, _ := modelTestFixture(t, true)
	var prepared modelTestPrepared
	for range modelTestMaxActive {
		prepared = preparedModelTest(t, a, modelTestInput(file))
	}
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, modelTestInput(file))}); response.StatusCode != 429 {
		t.Fatal("global concurrency cap bypassed")
	}
	body := `{"choices":[{"finish_reason":"stop","message":{"content":"Reasoning omitted.\nFinal answer: 21"}}],"usage":{"total_tokens":999999},"latency":999,"error":"dummy-private-error"}`
	response := a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": prepared.TestID, "status_code": 200, "body": body})})
	var result modelTestResult
	_ = json.Unmarshal(response.Body, &result)
	if response.StatusCode != 200 || result.Outcome != "completed" || len(result.Assertions) != 1 || !result.Assertions[0].Passed || result.UsageAvailable {
		t.Fatalf("result=%d %+v", response.StatusCode, result)
	}
	if strings.Contains(string(response.Body), "999999") || strings.Contains(string(response.Body), "dummy-private-error") {
		t.Fatal("model test reconstructed telemetry or upstream failure details")
	}
}

func TestModelTestProtectedStateAndModes(t *testing.T) {
	a, file, _ := modelTestFixture(t, false)
	if err := a.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","models":["dummy-model"],"probe_accounts":["dummy-model-test-account"]}`)); err != nil {
		t.Fatal(err)
	}
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, modelTestInput(file))}); response.StatusCode != 409 {
		t.Fatal("protected missing state was bypassed")
	}
	a.turnState.Before("learn", file.ID, "dummy-model", nil)
	if err := a.turnState.Learn("learn", file.ID, "dummy-model", http.Header{turnstate.Header: {rpcTurnStateTemplate()}}); err != nil {
		t.Fatal(err)
	}
	prepared := preparedModelTest(t, a, modelTestInput(file))
	if len(prepared.APICall.Header[turnstate.Header]) != 292 {
		t.Fatal("valid state not attached to exact native request")
	}
	for _, patch := range []string{`{"inject_mode":"replace-only"}`, `{"inject_mode":"always","dry_run":true}`, `{"dry_run":false,"enabled":false}`} {
		if err := a.turnState.Update([]byte(patch)); err != nil {
			t.Fatal(err)
		}
		if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, modelTestInput(file))}); response.StatusCode != 409 {
			t.Fatalf("protected injection mode bypassed: %s", patch)
		}
	}
}

func TestModelTestResponseParsersAndAssertions(t *testing.T) {
	for _, tc := range []struct{ protocol, body, want string }{
		{"openai", `{"choices":[{"message":{"content":[{"type":"text","text":"first"},{"type":"text","text":" second"}]}}]}`, "first second"},
		{"openai-responses", `data: {"type":"response.output_text.delta","delta":"earlier"}` + "\n" + `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"final"}]}]}}`, "final"},
		{"claude", `{"content":[{"type":"thinking","text":"private reasoning"},{"type":"text","text":"answer"}]}`, "answer"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"thought":true,"text":"private"},{"text":"answer"}]}}]}`, "answer"},
	} {
		if got := modelTestOutput(tc.body, tc.protocol); got != tc.want {
			t.Fatalf("%s output=%q", tc.protocol, got)
		}
	}
	if modelTestEqualJSON("```json\n{\"a\":1}\n```", `{"a":1}`) || modelTestEqualJSON(`{"a":1,"extra":0}`, `{"a":1}`) || modelTestEqualJSON(`{"a":1} {"a":1}`, `{"a":1}`) {
		t.Fatal("invalid JSON instruction output passed")
	}
}

func TestModelTestExactJSONNumbersDoNotRoundOrExpandHugeExponents(t *testing.T) {
	for _, tc := range []struct {
		actual, expected string
		equal            bool
	}{
		{`{"total":42.0000000000000001}`, `{"total":42}`, false},
		{`{"total":42.0}`, `{"total":42}`, true},
		{`{"total":4.2e1}`, `{"total":42}`, true},
		{`{"total":42,"total":42.0}`, `{"total":42}`, false},
		{`{"total":"42"}`, `{"total":42}`, false},
		{`{"total":1e1000000000}`, `{"total":42}`, false},
		{`{"total":1e-1000000000}`, `{"total":0}`, false},
		{`{"total":0e1025}`, `{"total":0}`, false},
		{`{"total":1` + strings.Repeat("0", 256) + `}`, `{"total":1}`, false},
	} {
		if got := modelTestEqualJSON(tc.actual, tc.expected); got != tc.equal {
			t.Fatalf("comparison %s with %s = %t; want %t", tc.actual, tc.expected, got, tc.equal)
		}
	}
}

func TestModelTestManagementRoutesStayAdminOnly(t *testing.T) {
	a, _, _ := modelTestFixture(t, true)
	for _, path := range []string{"/model-tests", "/model-tests/prepare", "/model-tests/complete"} {
		response := a.routeResource(ManagementRequest{Method: http.MethodPost}, path)
		if response.StatusCode == 200 {
			t.Fatalf("model test exposed as account resource: %s", path)
		}
	}
	response := a.getModelTests(ManagementRequest{})
	if response.StatusCode != 200 || !strings.Contains(string(response.Body), "dummy-model-index") || strings.Contains(string(response.Body), "dummy-secret-access") {
		t.Fatal("invalid private account catalog")
	}
}

func TestModelTestNativeSnapshotFallbackIsExplicitAndValidated(t *testing.T) {
	a, file, _ := modelTestFixture(t, true)
	a.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.get_runtime" {
			t.Fatalf("unexpected callback %s", method)
		}
		return nil, errors.New("configuration credentials are not listed")
	})
	input := modelTestInput(file)
	disabled := false
	input.Config.Provider = "openai-compatible-test"
	input.Config.AuthID = "openai-compatibility:test:0123456789ab"
	input.Config.AuthIndex = input.AuthIndex
	input.Config.CredentialRef = billing.CredentialFingerprint(input.Config.AuthID)
	input.Config.Disabled = &disabled
	prepared := preparedModelTest(t, a, input)
	if prepared.Account.IdentitySource != "management-snapshot" || prepared.Account.CredentialRef != input.Config.CredentialRef || prepared.APICall.AuthIndex != input.AuthIndex {
		t.Fatal("snapshot identity falsely reported as runtime-verified")
	}
	for _, change := range []func(*modelTestPrepareInput){
		func(value *modelTestPrepareInput) { value.Config.AuthIndex = "other-index" },
		func(value *modelTestPrepareInput) {
			value.Config.CredentialRef = billing.CredentialFingerprint("other-id")
		},
		func(value *modelTestPrepareInput) {
			value.Config.AuthID = "codex:apikey:0123456789ab"
			value.Config.CredentialRef = billing.CredentialFingerprint(value.Config.AuthID)
		},
		func(value *modelTestPrepareInput) { value.Config.Disabled = nil },
	} {
		next := input
		cfg := *input.Config
		next.Config = &cfg
		change(&next)
		if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, next)}); response.StatusCode != 409 {
			t.Fatalf("unsafe descriptor accepted: %d %s", response.StatusCode, response.Body)
		}
	}
	a.credentialRefsByIndex[input.AuthIndex] = billing.CredentialFingerprint("conflicting-real-observation")
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 409 {
		t.Fatal("snapshot overrode a real host index observation")
	}
}

func TestModelTestIncompleteOutputsCannotPassAssertions(t *testing.T) {
	for _, terminal := range []string{"", `data: {"type":"response.failed"}`, `data: {"type":"response.incomplete"}`} {
		a, file, _ := modelTestFixture(t, false)
		input := modelTestInput(file)
		prepared := preparedModelTest(t, a, input)
		delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": "Reasoning omitted.\nFinal answer: 21"})
		body := "data: " + string(delta) + "\n" + terminal
		response := a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": prepared.TestID, "status_code": 200, "body": body})})
		var result modelTestResult
		_ = json.Unmarshal(response.Body, &result)
		if result.Outcome != "incomplete" || !result.OutputTruncated || result.Output == "" || len(result.Assertions) != 1 || result.Assertions[0].Passed {
			t.Fatalf("partial SSE passed: %+v", result)
		}
	}
	for _, tc := range []struct{ protocol, body string }{
		{"openai", `{"choices":[{"finish_reason":"length","message":{"content":"text"}}]}`},
		{"openai", `{"choices":[{"finish_reason":"content_filter","message":{"content":"text"}}]}`},
		{"openai-responses", `{"status":"incomplete","output_text":"text"}`},
		{"claude", `{"stop_reason":"max_tokens","content":[{"type":"text","text":"text"}]}`},
		{"gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"parts":[{"text":"text"}]}}]}`},
	} {
		if output, complete := modelTestReadOutput(tc.body, tc.protocol); output != "text" || complete {
			t.Fatalf("incomplete %s result accepted", tc.protocol)
		}
	}
	if modelTestEqualJSON(`{"a":0,"a":1}`, `{"a":1}`) {
		t.Fatal("duplicate JSON keys passed an exact-object instruction")
	}
}

func TestModelTestIntegrationChannelHeadersAreSupported(t *testing.T) {
	for _, headers := range []map[string]string{
		{"Accept": "application/json", "User-Agent": "OpenCode/dummy", "x-opencode-session": "dummy-session"},
		{"x-client-type": "cline", "x-client-version": "1.0.0", "x-core-version": "1.0.0"},
	} {
		if _, err := modelTestHeaders(headers); err != nil {
			t.Fatal("our published integration channel headers were rejected", err)
		}
	}
	for _, header := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization", "Cookie"} {
		if _, err := modelTestHeaders(map[string]string{header: "dummy-secret"}); err == nil {
			t.Fatalf("secret header %s accepted", header)
		}
	}
}

func TestModelTestCatalogShowsAFileProxyWithoutCredentials(t *testing.T) {
	a, file, _ := modelTestFixture(t, false)
	response := a.getModelTests(ManagementRequest{})
	var catalog struct {
		Accounts []modelTestAccount `json:"accounts"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &catalog) != nil || len(catalog.Accounts) != 1 {
		t.Fatalf("catalog=%d %s", response.StatusCode, response.Body)
	}
	if account := catalog.Accounts[0]; account.AuthIndex != file.AuthIndex || account.Proxy != "socks5://***@proxy.invalid:1080" {
		t.Fatalf("account proxy = %+v", account)
	}
	if strings.Contains(string(response.Body), "dummy-password") || strings.Contains(string(response.Body), "dummy-user") {
		t.Fatal("the catalog exposed proxy credentials")
	}
}
