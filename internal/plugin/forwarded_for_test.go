package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

const dummyForwardedFor = "203.0.113.7"

func forwardedForHeaders() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "X-Forwarded-For": {dummyForwardedFor}}
}

func forwardedForApp(t *testing.T) *App {
	t.Helper()
	app := newConfiguredApp(t)
	if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
		t.Fatal(err)
	}
	return app
}

func putForwardedForBlock(t *testing.T, app *App, body map[string]any) billing.ForwardedForBlock {
	t.Helper()
	var result struct {
		ForwardedForBlock billing.ForwardedForBlock `json:"forwarded_for_block"`
		View              struct {
			ForwardedForBlock billing.ForwardedForBlock `json:"forwarded_for_block"`
		} `json:"view"`
	}
	callOK(t, app, http.MethodPut, routeForwardedForBlock, url.Values{"view": {"1"}}, map[string]any{"data": body}, http.StatusOK, &result)
	if !reflect.DeepEqual(result.View.ForwardedForBlock, result.ForwardedForBlock) {
		t.Fatalf("view = %+v, saved = %+v", result.View.ForwardedForBlock, result.ForwardedForBlock)
	}
	return result.ForwardedForBlock
}

func forwardedForIntercept(t *testing.T, app *App, req RequestInterceptRequest) RequestInterceptResponse {
	t.Helper()
	if req.Metadata == nil {
		req.Metadata = map[string]any{MetadataCallerScope: flowScope(), MetadataRequestPath: "/v1/chat/completions"}
	}
	raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, req))
	if err != nil {
		t.Fatalf("request.intercept_before error = %v", err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}

func forwardedForLogs(t *testing.T, app *App) []billing.PluginLog {
	t.Helper()
	page, err := app.store.PluginLogsPage(billing.PluginLogQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var kept []billing.PluginLog
	for _, entry := range page.Entries {
		if strings.HasPrefix(entry.Message, "X-Forwarded-For 拦截：") {
			kept = append(kept, entry)
		}
	}
	return kept
}

func assertForwardedForRefusal(t *testing.T, response RequestInterceptResponse, format, message string) {
	t.Helper()
	if !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %+v, want 403 refusal", response)
	}
	var payload struct {
		Type  string            `json:"type"`
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(response.ResponseBody, &payload); err != nil {
		t.Fatalf("invalid refusal body: %v (%s)", err, response.ResponseBody)
	}
	if payload.Error["type"] != "permission_error" || payload.Error["code"] != "access_denied" || payload.Error["message"] != message {
		t.Fatalf("refusal body = %s", response.ResponseBody)
	}
	if (format == "claude") != (payload.Type == "error") {
		t.Fatalf("%s refusal envelope = %s", format, response.ResponseBody)
	}
	if strings.Contains(string(response.ResponseBody), dummyForwardedFor) {
		t.Fatalf("refusal leaked the forwarded address: %s", response.ResponseBody)
	}
}

func TestForwardedForBlockManagementAPI(t *testing.T) {
	app := forwardedForApp(t)
	response := callManagement(t, app, http.MethodGet, routeForwardedForBlock, nil, nil)
	want := `{"forwarded_for_block":{"enabled":false,"model_keywords":[],"message":"` + billing.DefaultForwardedForBlockMessage + `"}}`
	if response.StatusCode != http.StatusOK || strings.TrimSpace(string(response.Body)) != want {
		t.Fatalf("GET defaults = %d %s", response.StatusCode, response.Body)
	}

	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("model-%d", i)
	}
	for name, body := range map[string]any{
		"empty body":               `{}`,
		"missing enabled":          map[string]any{"model_keywords": []string{"gpt"}},
		"non-boolean enabled":      `{"enabled":"yes","model_keywords":[]}`,
		"missing keywords":         `{"enabled":false}`,
		"null keywords":            `{"enabled":false,"model_keywords":null}`,
		"string keywords":          `{"enabled":false,"model_keywords":"gpt"}`,
		"enabled without keywords": map[string]any{"enabled": true, "model_keywords": []string{" ", ""}},
		"keyword too long":         map[string]any{"enabled": true, "model_keywords": []string{strings.Repeat("g", 513)}},
		"too many keywords":        map[string]any{"enabled": true, "model_keywords": tooMany},
		"message too long":         map[string]any{"enabled": false, "model_keywords": []string{}, "message": strings.Repeat("m", 1025)},
		"unknown field":            `{"enabled":false,"model_keywords":[],"deny":true}`,
	} {
		response := callManagement(t, app, http.MethodPut, routeForwardedForBlock, nil, body)
		var payload struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if err := json.Unmarshal(response.Body, &payload); err != nil || response.StatusCode != http.StatusBadRequest || payload.Error.Code != "invalid" {
			t.Fatalf("%s: status = %d, body = %s", name, response.StatusCode, response.Body)
		}
		if name == "enabled without keywords" && payload.Error.Message != "启用 X-Forwarded-For 拦截时至少填写一个模型关键词" {
			t.Fatalf("enabled without keywords message = %q", payload.Error.Message)
		}
	}
	if got := app.store.ForwardedForBlock(); !reflect.DeepEqual(got, billing.DefaultForwardedForBlock()) {
		t.Fatalf("rejected writes changed settings: %+v", got)
	}

	var viewResult map[string]map[string]json.RawMessage
	callOK(t, app, http.MethodPut, routeForwardedForBlock, url.Values{"view": {"1"}}, map[string]any{"data": map[string]any{
		"enabled": true, "model_keywords": []string{" gpt ", "GPT", "", "Claude"}, "message": "  禁止混用  ",
	}}, http.StatusOK, &viewResult)
	if len(viewResult["view"]) != 1 || viewResult["view"]["forwarded_for_block"] == nil {
		t.Fatalf("view = %s", mustMarshal(t, viewResult["view"]))
	}
	saved := billing.ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt", "Claude"}, Message: "禁止混用"}
	var viewed billing.ForwardedForBlock
	if err := json.Unmarshal(viewResult["view"]["forwarded_for_block"], &viewed); err != nil || !reflect.DeepEqual(viewed, saved) {
		t.Fatalf("view settings = %+v, %v", viewed, err)
	}

	var plain struct {
		ForwardedForBlock billing.ForwardedForBlock `json:"forwarded_for_block"`
	}
	callOK(t, app, http.MethodPut, routeForwardedForBlock, nil, map[string]any{"enabled": false, "model_keywords": []string{"gpt"}}, http.StatusOK, &plain)
	if want := (billing.ForwardedForBlock{ModelKeywords: []string{"gpt"}, Message: billing.DefaultForwardedForBlockMessage}); !reflect.DeepEqual(plain.ForwardedForBlock, want) {
		t.Fatalf("omitted message = %+v", plain.ForwardedForBlock)
	}
	plain.ForwardedForBlock = billing.ForwardedForBlock{}
	callOK(t, app, http.MethodGet, routeForwardedForBlock, nil, nil, http.StatusOK, &plain)
	if plain.ForwardedForBlock.Enabled || !reflect.DeepEqual(plain.ForwardedForBlock.ModelKeywords, []string{"gpt"}) {
		t.Fatalf("GET after save = %+v", plain.ForwardedForBlock)
	}
}

func TestForwardedForBlockRefusesMatchingModelsWithHeader(t *testing.T) {
	app := forwardedForApp(t)
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"GPT"}, "message": "禁止混用"})
	helper := map[string]any{MetadataCallerScope: flowScope(), MetadataSource: SourcePluginHostModelCallback}
	for _, test := range []struct {
		name    string
		req     RequestInterceptRequest
		blocked bool
	}{
		{name: "openai", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", RequestedModel: "gpt-5.5", Headers: forwardedForHeaders()}, blocked: true},
		{name: "claude", req: RequestInterceptRequest{SourceFormat: "claude", Model: "gpt-5.5", RequestedModel: "gpt-5.5", Headers: forwardedForHeaders()}, blocked: true},
		{name: "lower-case header", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", Headers: http.Header{"x-forwarded-for": {dummyForwardedFor}}}, blocked: true},
		{name: "empty header value", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", Headers: http.Header{"X-Forwarded-For": {""}}}, blocked: true},
		{name: "requested model only", req: RequestInterceptRequest{SourceFormat: "openai", Model: "claude-opus-5", RequestedModel: "team-gpt-alias", Headers: forwardedForHeaders()}, blocked: true},
		{name: "execution model only", req: RequestInterceptRequest{SourceFormat: "openai", Model: "openai/gpt-4o", RequestedModel: "auto", Headers: forwardedForHeaders()}, blocked: true},
		{name: "header without values", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", Headers: http.Header{"X-Forwarded-For": {}}}},
		{name: "no header", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", RequestedModel: "gpt-5.5", Headers: http.Header{"X-Real-Ip": {dummyForwardedFor}}}},
		{name: "non-matching model", req: RequestInterceptRequest{SourceFormat: "openai", Model: "claude-opus-5", RequestedModel: "claude-opus-5", Headers: forwardedForHeaders()}},
		{name: "helper request", req: RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", RequestedModel: "gpt-5.5", Headers: forwardedForHeaders(), Metadata: helper}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := forwardedForIntercept(t, app, test.req)
			if test.blocked {
				assertForwardedForRefusal(t, response, test.req.SourceFormat, "禁止混用")
			} else if response.Terminate {
				t.Fatalf("request was refused: %d %s", response.StatusCode, response.ResponseBody)
			}
		})
	}
}

func TestForwardedForBlockTogglesWithoutRestart(t *testing.T) {
	app, statePath := newAppWithPriceAndState(t, true)
	if _, err := app.store.SyncKeys([]string{testAPIKey}, false); err != nil {
		t.Fatal(err)
	}
	request := func(model string) RequestInterceptResponse {
		return forwardedForIntercept(t, app, RequestInterceptRequest{SourceFormat: "openai", Model: model, RequestedModel: model, Headers: forwardedForHeaders()})
	}
	if response := request("gpt-5.5"); response.Terminate {
		t.Fatalf("disabled default refused a request: %s", response.ResponseBody)
	}
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}})
	assertForwardedForRefusal(t, request("gpt-5.5"), "openai", billing.DefaultForwardedForBlockMessage)
	putForwardedForBlock(t, app, map[string]any{"enabled": false, "model_keywords": []string{"gpt"}})
	if response := request("gpt-5.5"); response.Terminate {
		t.Fatalf("disabled rule refused a request: %s", response.ResponseBody)
	}
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"claude"}, "message": "仅限直连"})
	if response := request("gpt-5.5"); response.Terminate {
		t.Fatalf("old keyword still refused: %s", response.ResponseBody)
	}
	assertForwardedForRefusal(t, request("claude-opus-5"), "openai", "仅限直连")

	// The saved rule is also what a restarted plugin loads.
	app.Shutdown()
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	raw, err := restarted.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{
		ConfigYAML: []byte("enabled: true\nstate_file: \"" + statePath + "\"\n"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, nil)
	if got := restarted.store.ForwardedForBlock(); !got.Enabled || !reflect.DeepEqual(got.ModelKeywords, []string{"claude"}) || got.Message != "仅限直连" {
		t.Fatalf("restarted settings = %+v", got)
	}
}

func TestForwardedForBlockRunsBeforeSlotsAndQuota(t *testing.T) {
	app := concurrencyApp(t, 1)
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{flowModel}})
	metadata := map[string]any{MetadataCallerScope: flowScope(), MetadataRequestPath: "/v1/responses", MetadataGenerate: true}
	blocked := forwardedForIntercept(t, app, RequestInterceptRequest{
		RequestID: "forwarded-1", SourceFormat: "openai", Model: flowModel, RequestedModel: flowModel,
		Headers: forwardedForHeaders(), Metadata: metadata,
	})
	assertForwardedForRefusal(t, blocked, "openai", billing.DefaultForwardedForBlockMessage)
	if current := app.store.KeyViews()[0].CurrentConcurrency; current != 0 {
		t.Fatalf("CurrentConcurrency = %d, want no slot taken by the refused request", current)
	}
	if admitted := interceptWithID(t, app, "direct-1", true); admitted.Terminate {
		t.Fatalf("request without the header = %+v", admitted)
	}
	if current := app.store.KeyViews()[0].CurrentConcurrency; current != 1 {
		t.Fatalf("CurrentConcurrency = %d, want 1", current)
	}
	// With the only slot now taken, the rule still answers first.
	blocked = forwardedForIntercept(t, app, RequestInterceptRequest{
		RequestID: "forwarded-2", SourceFormat: "openai", Model: flowModel, RequestedModel: flowModel,
		Headers: forwardedForHeaders(), Metadata: metadata,
	})
	assertForwardedForRefusal(t, blocked, "openai", billing.DefaultForwardedForBlockMessage)
	completeRequest(t, app, "direct-1")

	exhausted := exhaustedApp(t, time.Hour)
	putForwardedForBlock(t, exhausted, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}})
	response := forwardedForIntercept(t, exhausted, RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", Headers: forwardedForHeaders()})
	assertForwardedForRefusal(t, response, "openai", billing.DefaultForwardedForBlockMessage)
	if response := callIntercept(t, exhausted, "openai"); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quota refusal without the header = %+v", response)
	}
}

func TestForwardedForBlockLogsOncePerKeyword(t *testing.T) {
	app := forwardedForApp(t)
	if err := app.store.SetLabel(flowScope(), "Alice"); err != nil {
		t.Fatal(err)
	}
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}})
	request := func(model string) {
		t.Helper()
		response := forwardedForIntercept(t, app, RequestInterceptRequest{SourceFormat: "openai", Model: model, RequestedModel: model, Headers: forwardedForHeaders()})
		assertForwardedForRefusal(t, response, "openai", billing.DefaultForwardedForBlockMessage)
	}
	for range 3 {
		request("gpt-5.5")
	}
	logs := forwardedForLogs(t, app)
	if len(logs) != 1 || logs[0].Level != billing.PluginLogInfo {
		t.Fatalf("logs = %+v, want one information entry", logs)
	}
	for _, want := range []string{"Alice", "/v1/chat/completions", "模型 gpt-5.5", "命中关键词 gpt"} {
		if !strings.Contains(logs[0].Message, want) {
			t.Fatalf("log = %q, want %q", logs[0].Message, want)
		}
	}
	for _, leaked := range []string{dummyForwardedFor, testAPIKey, flowScope()} {
		if strings.Contains(logs[0].Message, leaked) {
			t.Fatalf("log = %q leaked %q", logs[0].Message, leaked)
		}
	}
	// A relay mixing models, including ever-changing suffixes, stays one entry.
	for i := range 20 {
		request(fmt.Sprintf("gpt-5-mini(%d)", i))
		request("gpt-4o")
	}
	if logs := forwardedForLogs(t, app); len(logs) != 1 {
		t.Fatalf("logs = %d entries, want alternating models reported once", len(logs))
	}
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}})
	request("gpt-4o")
	if logs := forwardedForLogs(t, app); len(logs) != 2 {
		t.Fatalf("logs = %+v, want saving the settings to report again", logs)
	}
}

func TestForwardedForBlockLogsBoundedModelNames(t *testing.T) {
	app := forwardedForApp(t)
	putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}})
	const secret = "sk-dummyforgedlogsecret0123456789"
	model := "gpt-5(\n[ERROR] forged line " + secret + " " + strings.Repeat("x", 4096) + ")"
	response := forwardedForIntercept(t, app, RequestInterceptRequest{SourceFormat: "openai", Model: model, RequestedModel: model, Headers: forwardedForHeaders()})
	assertForwardedForRefusal(t, response, "openai", billing.DefaultForwardedForBlockMessage)
	logs := forwardedForLogs(t, app)
	if len(logs) != 1 {
		t.Fatalf("logs = %+v, want one entry", logs)
	}
	message := logs[0].Message
	if strings.ContainsAny(message, "\r\n") || strings.Contains(message, secret) || !strings.Contains(message, "模型 gpt-5([ERROR] forged line ") {
		t.Fatalf("log = %q, want one line with the key-like token masked", message)
	}
	if len(message) > 512 || !strings.HasSuffix(message, "…，命中关键词 gpt") {
		t.Fatalf("log length = %d, want the model truncated: %q", len(message), message)
	}
	if got := forwardedForLogModel("claude-sonnet-4-5-20250929"); got != "claude-sonnet-4-5-20250929" {
		t.Fatalf("dated model name = %q, want it readable", got)
	}
}

func TestForwardedForBlockIgnoresAccessControl(t *testing.T) {
	gpt := RequestInterceptRequest{SourceFormat: "openai", Model: "gpt-5.5", RequestedModel: "gpt-5.5", Headers: forwardedForHeaders()}
	direct := gpt
	direct.Headers = http.Header{"Content-Type": {"application/json"}}

	t.Run("access control off", func(t *testing.T) {
		app := forwardedForApp(t)
		putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}, "message": "禁止混用"})
		if err := app.store.SetAccessControl(billing.AccessControl{}); err != nil {
			t.Fatal(err)
		}
		assertForwardedForRefusal(t, forwardedForIntercept(t, app, gpt), "openai", "禁止混用")
		if response := forwardedForIntercept(t, app, direct); response.Terminate {
			t.Fatalf("request without the header was refused: %d %s", response.StatusCode, response.ResponseBody)
		}
	})

	t.Run("answers before routing", func(t *testing.T) {
		app := forwardedForApp(t)
		putForwardedForBlock(t, app, map[string]any{"enabled": true, "model_keywords": []string{"gpt"}, "message": "禁止混用"})
		if err := app.store.SetAccessControl(billing.AccessControl{Enabled: true, DenyUngrouped: true}); err != nil {
			t.Fatal(err)
		}
		// The ungrouped key would be refused by routing, with another message.
		if response := forwardedForIntercept(t, app, direct); !response.Terminate || strings.Contains(string(response.ResponseBody), "禁止混用") {
			t.Fatalf("routing refusal = %d %s", response.StatusCode, response.ResponseBody)
		}
		assertForwardedForRefusal(t, forwardedForIntercept(t, app, gpt), "openai", "禁止混用")
	})
}
