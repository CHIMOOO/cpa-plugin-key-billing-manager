package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelTestReadsOnlyProtocolModelDeclarations(t *testing.T) {
	for _, tc := range []struct{ protocol, body, model, status string }{
		{"openai", `{"model":"upstream-model","choices":[{"message":{"content":"model: fake"}}]}`, "upstream-model", "matched"},
		{"openai-responses", `{"model":"versioned-model"}`, "versioned-model", "different"},
		{"claude", `{"model":"claude-version"}`, "claude-version", "different"},
		{"gemini", `{"modelVersion":"upstream-model","model":"ignore"}`, "upstream-model", "matched"},
		{"openai", `{"choices":[{"message":{"content":"upstream-model"}}]}`, "", "missing"},
		{"openai", `{"model":"bad\nname"}`, "", "missing"},
		{"openai", `{"model":"` + strings.Repeat("x", 257) + `"}`, "", "missing"},
		{"openai-responses", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"upstream-model\"}}\n\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"model\":\"upstream-model\"}}\n\n", "upstream-model", "matched"},
		{"openai-responses", "data: {\"type\":\"response.created\",\"response\":{\"model\":\"one\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"two\"}}\n\n", "", "conflicting"},
		{"openai-responses", "data: {\"type\":\"response.output_text.delta\",\"response\":{\"model\":\"fake\"},\"delta\":\"upstream-model\"}\n\n", "", "missing"},
	} {
		model, status := modelTestDeclaredModel(tc.body, tc.protocol, "upstream-model")
		if model != tc.model || status != tc.status {
			t.Fatalf("%s: got %q/%s want %q/%s", tc.protocol, model, status, tc.model, tc.status)
		}
	}
}

func TestModelTestDeclarationComparesResolvedAliasAndDoesNotReusePriorResult(t *testing.T) {
	a, file, _ := modelTestFixture(t, true)
	input := modelTestInput(file)
	input.Model = "friendly-alias"
	input.Config.Models = append(input.Config.Models, struct {
		Name  string `json:"name"`
		Alias string `json:"alias,omitempty"`
	}{Name: "actual-upstream", Alias: "friendly-alias"})
	for _, declared := range []string{"actual-upstream", "other-version", ""} {
		prepared := preparedModelTest(t, a, input)
		body := map[string]any{"model": declared, "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": "Reasoning omitted.\nFinal answer: 21"}}}}
		response := a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": prepared.TestID, "status_code": 200, "body": string(mustMarshal(t, body))})})
		var result modelTestResult
		if json.Unmarshal(response.Body, &result) != nil || result.RequestedModel != "friendly-alias" || result.UpstreamModel != "actual-upstream" || result.ResponseModel != declared || !result.Assertions[0].Passed || result.UsageAvailable {
			t.Fatalf("model metadata altered diagnosis: %s", response.Body)
		}
		want := "matched"
		if declared == "other-version" {
			want = "different"
		}
		if declared == "" {
			want = "missing"
		}
		if result.ResponseModelStatus != want {
			t.Fatalf("stale model status: %+v", result)
		}
	}
}
