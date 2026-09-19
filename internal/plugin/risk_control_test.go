package plugin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRiskCenterLocalRulesHashMemoryAndRedaction(t *testing.T) {
	a := newConfiguredApp(t)
	config := map[string]any{"enabled": true, "mode": "observe", "blocked_keywords": []string{"dummy-risk-word"}}
	if got := a.setRiskConfig(ManagementRequest{Body: mustMarshal(t, config)}); got.StatusCode != 200 {
		t.Fatal(string(got.Body))
	}
	req := RequestInterceptRequest{Model: "gpt-model", Metadata: map[string]any{MetadataCallerScope: "dummy-private-key"}, Body: []byte(`{"messages":[{"role":"user","content":"private prompt prefix DUMMY-risk-word suffix"}]}`)}
	if a.risk.inspect(req).Terminate {
		t.Fatal("observe mode blocked")
	}
	config["mode"], config["blocked_keywords"] = "pre_block", []string{}
	if got := a.setRiskConfig(ManagementRequest{Body: mustMarshal(t, config)}); got.StatusCode != 200 {
		t.Fatal(string(got.Body))
	}
	if got := a.risk.inspect(req); !got.Terminate || got.StatusCode != 403 {
		t.Fatal("remembered input not blocked", got)
	}
	raw, _ := os.ReadFile(a.risk.path)
	for _, secret := range []string{"private prompt prefix", "dummy-private-key", "DUMMY-risk-word"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("risk storage disclosed request data")
		}
	}
	var view struct {
		Status map[string]int `json:"status"`
		Events []riskEvent    `json:"events"`
	}
	json.Unmarshal(a.getRiskCenter(ManagementRequest{}).Body, &view)
	if view.Status["observed"] != 1 || view.Status["blocked"] != 1 || view.Status["hash_hits"] != 1 {
		t.Fatal(view)
	}
	loaded, err := loadRiskControl(a.risk.path)
	if err != nil || len(loaded.Hashes) != 1 || len(loaded.Events) != 2 {
		t.Fatal("risk persistence failed", err)
	}
	a.clearRiskHashes(ManagementRequest{})
	if a.risk.inspect(req).Terminate {
		t.Fatal("cleared hashes still blocked")
	}
	a.clearRiskEvents(ManagementRequest{})
	if len(a.risk.state.Events) != 0 {
		t.Fatal("events not cleared")
	}
}

func TestRiskCenterRejectsUninspectablePayloadOnlyInRelevantBlockingMode(t *testing.T) {
	a := newConfiguredApp(t)
	config := map[string]any{"enabled": true, "mode": "pre_block", "model_filter": riskModelFilter{Mode: "include", Models: []string{"protected-model"}}}
	a.setRiskConfig(ManagementRequest{Body: mustMarshal(t, config)})
	for _, body := range [][]byte{nil, []byte(`not-json`), []byte(`{"unknown":"uninspected text"}`), []byte(strings.Repeat(" ", 1<<20+1))} {
		if !a.risk.inspect(RequestInterceptRequest{Model: "protected-model", Body: body}).Terminate {
			t.Fatal("uninspectable body escaped")
		}
		if a.risk.inspect(RequestInterceptRequest{Model: "other-model", Body: body}).Terminate {
			t.Fatal("model scope ignored")
		}
	}
	config["mode"] = "observe"
	a.setRiskConfig(ManagementRequest{Body: mustMarshal(t, config)})
	if a.risk.inspect(RequestInterceptRequest{Model: "protected-model"}).Terminate {
		t.Fatal("observe mode refused missing body")
	}
	if a.risk.state.Events[len(a.risk.state.Events)-1].Decision != "not_inspected" {
		t.Fatal("missing body falsely screened")
	}
	a.risk.now = func() time.Time { return time.Now().Add(366 * 24 * time.Hour) }
	a.getRiskCenter(ManagementRequest{})
	if len(a.risk.state.Events) != 0 {
		t.Fatal("retention not enforced")
	}
}

func TestRiskPolicyFailedSaveKeepsPreviousPolicy(t *testing.T) {
	a := newConfiguredApp(t)
	before := a.risk.state.Config
	response := a.setRiskConfig(ManagementRequest{Body: []byte(`{"blocked_keywords":["new-value"],"mode":"invalid"}`)})
	if response.StatusCode != 400 || len(a.risk.state.Config.BlockedKeywords) != 0 || a.risk.state.Config.Mode != before.Mode {
		t.Fatal("failed validation modified policy")
	}
	a.risk.path = t.TempDir()
	response = a.setRiskConfig(ManagementRequest{Body: []byte(`{"enabled":true}`)})
	if response.StatusCode != 500 || a.risk.state.Config.Enabled {
		t.Fatal("failed persistence modified policy")
	}
}

func TestRiskFullHistoryRemainsReloadable(t *testing.T) {
	a := newConfiguredApp(t)
	keywords := make([]string, 256)
	for i := range keywords {
		keywords[i] = strings.Repeat("x", i+1)
	}
	if response := a.setRiskConfig(ManagementRequest{Body: mustMarshal(t, map[string]any{"enabled": true, "mode": "pre_block", "max_events": 2000, "blocked_keywords": keywords})}); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	a.risk.inspect(RequestInterceptRequest{Model: strings.Repeat("<", 200), Body: mustMarshal(t, map[string]string{"input": strings.Repeat("x", 256)})})
	event := a.risk.state.Events[0]
	for len(a.risk.state.Events) < 2000 {
		a.risk.state.Events = append(a.risk.state.Events, event)
	}
	for len(a.risk.state.Hashes) < 4096 {
		a.risk.state.Hashes = append(a.risk.state.Hashes, riskDigest("dummy hash"))
	}
	if err := writePrivateJSON(a.risk.path, a.risk.state); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadRiskControl(a.risk.path)
	if err != nil || len(loaded.Events) != 2000 {
		t.Fatal("a full generated risk history cannot reload", err)
	}
}

func TestRiskInspectResponsesFunctionCallOutput(t *testing.T) {
	a := newConfiguredApp(t)
	if response := a.setRiskConfig(ManagementRequest{Body: []byte(`{"enabled":true,"mode":"pre_block","blocked_keywords":["BLOCKED_KEYWORD"]}`)}); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	for _, output := range []any{"BLOCKED_KEYWORD", []any{map[string]any{"type": "input_text", "text": "BLOCKED_KEYWORD"}}} {
		body := mustMarshal(t, map[string]any{"instructions": "hello", "input": []any{map[string]any{"type": "function_call_output", "call_id": "dummy", "output": output}}})
		if response := a.risk.inspect(RequestInterceptRequest{Model: "model", Body: body}); !response.Terminate {
			t.Fatal("tool output bypassed pre-block")
		}
	}
}

func TestRiskModelScopesNormalizeThinkingSuffixAndKeepPrefixes(t *testing.T) {
	for _, tc := range []struct {
		rule, model string
		match       bool
	}{
		{"gpt-5.6-sol", "gpt-5.6-sol(high)", true},
		{"gpt-5.6-sol(high)", "gpt-5.6-sol", true},
		{"gpt-5.6-sol", "gpt-5.6-sol(1024)", true},
		{"gpt-5.6-sol", "gpt-5.6-sol(custom)", false},
		{"team/gpt-5.6-sol", "team/gpt-5.6-sol(high)", true},
		{"team/gpt-5.6-sol", "other/gpt-5.6-sol(high)", false},
		{"gpt-5.6-sol", "team/gpt-5.6-sol(high)", false},
	} {
		for _, mode := range []string{"include", "exclude"} {
			config := defaultRiskConfig()
			config.ModelFilter = riskModelFilter{Mode: mode, Models: []string{tc.rule}}
			want := tc.match
			if mode == "exclude" {
				want = !want
			}
			if got := riskModelMatches(config, tc.model); got != want {
				t.Fatalf("mode=%s rule=%s model=%s matches=%v want=%v", mode, tc.rule, tc.model, got, want)
			}
		}
	}
}

func TestRiskInspectsExplicitGeminiAndCustomToolOutputs(t *testing.T) {
	a := newConfiguredApp(t)
	if response := a.setRiskConfig(ManagementRequest{Body: []byte(`{"enabled":true,"mode":"pre_block","blocked_keywords":["BLOCKED_KEYWORD"]}`)}); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	for _, body := range []string{
		`{"contents":[{"parts":[{"text":"hello"},{"functionResponse":{"name":"dummy","response":{"custom":{"message":"BLOCKED_KEYWORD"}}}}]}]}`,
		`{"instructions":"hello","input":[{"type":"custom_tool_call_output","call_id":"dummy","output":"BLOCKED_KEYWORD"}]}`,
		`{"contents":[{"parts":[{"text":"hello"},{"functionResponse":{"name":"dummy"}}]}]}`,
	} {
		if response := a.risk.inspect(RequestInterceptRequest{Model: "model", Body: []byte(body)}); !response.Terminate {
			t.Fatal("explicit tool response was silently partially inspected")
		}
	}
	first, _ := riskText([]byte(`{"contents":[{"parts":[{"functionResponse":{"response":{"a":"first","b":"second"}}}]}]}`))
	second, _ := riskText([]byte(`{"contents":[{"parts":[{"functionResponse":{"response":{"b":"second","a":"first"}}}]}]}`))
	if first != second {
		t.Fatal("tool object key order changed hash memory")
	}
}
