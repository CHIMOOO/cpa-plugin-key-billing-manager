package plugin

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"cpa-key-billing/internal/messages"
)

type modelTestPresetSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type modelTestPreset struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Prompt      string                  `json:"prompt"`
	Assertion   string                  `json:"assertion"`
	Expected    string                  `json:"expected,omitempty"`
	LabelKey    string                  `json:"label_key"`
	CriteriaKey string                  `json:"criteria_key"`
	Sources     []modelTestPresetSource `json:"sources"`
}

// Canonical prompts and their sources are shared with the development preview.
// Checks concern the stated task only, not model identity or overall quality.
//
//go:embed model_test_presets.json
var modelTestPresetData []byte

var modelTestPresets = func() []modelTestPreset {
	var presets []modelTestPreset
	if err := json.Unmarshal(modelTestPresetData, &presets); err != nil {
		panic("invalid embedded model test presets")
	}
	return presets
}()

func modelTestPresetByID(id string) *modelTestPreset {
	for index := range modelTestPresets {
		if modelTestPresets[index].ID == id {
			return &modelTestPresets[index]
		}
	}
	return nil
}

func modelTestPrompt(preset, prompt, expected string) (string, string, error) {
	selected := modelTestPresetByID(preset)
	if selected == nil {
		return "", "", errors.New("Unknown model test preset")
	}
	if prompt == "" {
		prompt = selected.Prompt
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > modelTestPromptBytes || !utf8.ValidString(prompt) || strings.ContainsRune(prompt, 0) {
		return "", "", errors.New("The model test prompt must contain 1–8192 UTF-8 bytes")
	}
	if strings.Contains(prompt, "$TOKEN$") {
		return "", "", errors.New("The host token placeholder cannot be used inside a test prompt")
	}
	if len(expected) > 2048 || !utf8.ValidString(expected) || strings.ContainsRune(expected, 0) {
		return "", "", errors.New("The expected output is too large or invalid")
	}
	if preset != "free" && prompt != selected.Prompt {
		return "", "", errors.New("Use the custom preset after editing a test prompt")
	}
	if preset != "free" {
		expected = selected.Expected
	}
	return prompt, expected, nil
}

type modelTestAssertion struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Expected string `json:"expected,omitempty"`
}

type modelTestResult struct {
	TestID              string               `json:"test_id"`
	Outcome             string               `json:"outcome"`
	Output              string               `json:"output"`
	OutputTruncated     bool                 `json:"output_truncated"`
	Assertions          []modelTestAssertion `json:"assertions"`
	Reason              string               `json:"reason,omitempty"`
	ReasonMessage       messages.Message     `json:"reason_message,omitzero"`
	UsageAvailable      bool                 `json:"usage_available"`
	LeaseRetained       bool                 `json:"lease_retained,omitempty"`
	RequestedModel      string               `json:"requested_model"`
	UpstreamModel       string               `json:"upstream_model"`
	ResponseModel       string               `json:"response_model"`
	ResponseModelStatus string               `json:"response_model_status"`
}

// A management client's submitted completion is diagnostic display data, not
// trusted provider telemetry. Never reconstruct usage, latency, billing, or an
// upstream failure event from this body. usage.handle remains their sole source.
func (a *App) completeModelTest(req ManagementRequest) ManagementResponse {
	if len(req.Body) > modelTestMaxResponseBytes+4096 {
		return modelTestError(413, "The model test response is too large")
	}
	var input struct {
		TestID          string `json:"test_id"`
		StatusCode      int    `json:"status_code,omitempty"`
		Body            string `json:"body,omitempty"`
		TransportFailed bool   `json:"transport_failed,omitempty"`
		NotStarted      bool   `json:"not_started,omitempty"`
	}
	if decodeStrict(req.Body, &input) != nil || len(input.TestID) != 48 || len(input.Body) > modelTestMaxResponseBytes {
		return modelTestError(400, "Invalid model test completion")
	}
	if input.NotStarted && (input.StatusCode != 0 || input.Body != "" || input.TransportFailed) || !input.NotStarted && !input.TransportFailed && (input.StatusCode < 100 || input.StatusCode > 599) {
		return modelTestError(400, "Invalid model test completion state")
	}
	a.modelTestsMu.Lock()
	a.pruneModelTestsLocked(a.modelTestTime())
	lease, ok := a.modelTests[input.TestID]
	if !ok {
		a.modelTestsMu.Unlock()
		return modelTestError(409, "This model test expired or was already completed")
	}
	result := modelTestResult{TestID: input.TestID, Outcome: "failed", Assertions: []modelTestAssertion{},
		RequestedModel: lease.RequestedModel, UpstreamModel: lease.UpstreamModel, ResponseModelStatus: "missing"}
	if input.TransportFailed {
		// A disconnected browser cannot prove the host API-call stopped. Keep
		// its slot through the original bounded timeout instead of overlapping
		// another model request with the potentially still-running call.
		result.LeaseRetained = true
		result.Reason = "The management connection failed; its account slot remains reserved until the test lease expires"
		a.modelTestsMu.Unlock()
		return modelTestJSON(200, result)
	}
	delete(a.modelTests, input.TestID)
	a.accountRuntime.release(lease.RequestID)
	a.modelTestsMu.Unlock()
	if input.NotStarted {
		result.Reason = "The prepared test was cancelled before sending"
		return modelTestJSON(200, result)
	}
	if input.StatusCode < 200 || input.StatusCode >= 300 {
		result.Reason = "The native model test did not return a successful response"
		return modelTestJSON(200, result)
	}
	result.ResponseModel, result.ResponseModelStatus = modelTestDeclaredModel(input.Body, lease.Protocol, lease.UpstreamModel)
	output, complete := modelTestReadOutput(input.Body, lease.Protocol)
	if output == "" {
		result.Reason = "The response contains no supported model text output"
		return modelTestJSON(200, result)
	}
	result.Outcome = "completed"
	if !complete {
		result.Outcome = "incomplete"
		result.OutputTruncated = true
		result.Reason = "The model output did not finish; its assertions cannot pass"
	}
	if len(output) > 16384 {
		output = output[:16384]
		for !utf8.ValidString(output) {
			output = output[:len(output)-1]
		}
		result.OutputTruncated = true
	}
	result.Output = output
	if lease.Expected != "" {
		name := "Exact output"
		if preset := modelTestPresetByID(lease.Preset); preset != nil && (preset.Assertion == "Exact JSON object" || preset.Assertion == "Final answer") {
			name = preset.Assertion
		}
		passed := false
		if !result.OutputTruncated {
			switch name {
			case "Exact JSON object":
				passed = modelTestEqualJSON(output, lease.Expected)
			case "Final answer":
				passed = modelTestFinalAnswer(output) == lease.Expected
			default:
				passed = strings.TrimSpace(output) == strings.TrimSpace(lease.Expected)
			}
		}
		result.Assertions = append(result.Assertions, modelTestAssertion{Name: name, Passed: passed, Expected: lease.Expected})
	}
	return modelTestJSON(200, result)
}

var modelTestFinalAnswerPattern = regexp.MustCompile(`(?i)(?:最终答案|final answer)[*_\s]*[:：][*_\s]*(\d+)`)

// modelTestFinalAnswer returns the number on the last answer line the prompt
// asks for. A number merely mentioned in the reasoning never counts.
func modelTestFinalAnswer(output string) string {
	matches := modelTestFinalAnswerPattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return ""
	}
	return strings.TrimLeft(matches[len(matches)-1][1], "0")
}

func modelTestEqualJSON(actual, expected string) bool {
	parse := func(raw string) (any, bool) {
		uniqueDecoder := json.NewDecoder(strings.NewReader(raw))
		uniqueDecoder.UseNumber()
		if !modelTestUniqueJSON(uniqueDecoder, 0) {
			return nil, false
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return nil, false
		}
		return value, true
	}
	actualValue, ok := parse(actual)
	if !ok {
		return false
	}
	expectedValue, ok := parse(expected)
	return ok && modelTestJSONValuesEqual(actualValue, expectedValue)
}

func modelTestJSONValuesEqual(actual, expected any) bool {
	switch value := actual.(type) {
	case json.Number:
		other, ok := expected.(json.Number)
		if !ok {
			return false
		}
		left, leftOK := modelTestExactNumber(value)
		right, rightOK := modelTestExactNumber(other)
		return leftOK && rightOK && left.Cmp(right) == 0
	case map[string]any:
		other, ok := expected.(map[string]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for key, item := range value {
			expectedItem, exists := other[key]
			if !exists || !modelTestJSONValuesEqual(item, expectedItem) {
				return false
			}
		}
		return true
	case []any:
		other, ok := expected.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for index, item := range value {
			if !modelTestJSONValuesEqual(item, other[index]) {
				return false
			}
		}
		return true
	case string:
		other, ok := expected.(string)
		return ok && value == other
	case bool:
		other, ok := expected.(bool)
		return ok && value == other
	case nil:
		return expected == nil
	default:
		return false
	}
}

func modelTestExactNumber(number json.Number) (*big.Rat, bool) {
	raw := string(number)
	// Keep exact decimal comparisons bounded before allocating big integers.
	// Large exponents need not be expanded just to grade these small presets.
	if len(raw) > 256 {
		return nil, false
	}
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(raw[index+1:])
		if err != nil || exponent < -1024 || exponent > 1024 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(raw)
}

func modelTestUniqueJSON(decoder *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, container := token.(json.Delim)
	if !container {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return false
			}
			seen[name] = true
			if !modelTestUniqueJSON(decoder, depth+1) {
				return false
			}
		}
	case '[':
		for decoder.More() {
			if !modelTestUniqueJSON(decoder, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	_, err = decoder.Token()
	return err == nil
}

func modelTestTextParts(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	parts, _ := value.([]any)
	var output strings.Builder
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok || part["thought"] == true {
			continue
		}
		kind, _ := part["type"].(string)
		if kind == "" || kind == "text" || kind == "output_text" {
			text, _ := part["text"].(string)
			output.WriteString(text)
		}
	}
	return output.String()
}

func modelTestJSONOutput(body map[string]any, protocol string) string {
	switch protocol {
	case "openai":
		choices, _ := body["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			return modelTestTextParts(message["content"])
		}
	case "openai-responses":
		items, _ := body["output"].([]any)
		var output strings.Builder
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["type"] == "message" {
				output.WriteString(modelTestTextParts(item["content"]))
			}
		}
		if output.Len() > 0 {
			return output.String()
		}
		text, _ := body["output_text"].(string)
		return text
	case "claude":
		return modelTestTextParts(body["content"])
	case "gemini":
		candidates, _ := body["candidates"].([]any)
		if len(candidates) > 0 {
			candidate, _ := candidates[0].(map[string]any)
			content, _ := candidate["content"].(map[string]any)
			return modelTestTextParts(content["parts"])
		}
	}
	return ""
}

func modelTestOutput(raw, protocol string) string {
	text, _ := modelTestReadOutput(raw, protocol)
	return text
}

func modelTestReadOutput(raw, protocol string) (string, bool) {
	var body map[string]any
	if json.Unmarshal([]byte(raw), &body) == nil {
		complete := true
		switch protocol {
		case "openai":
			choices, _ := body["choices"].([]any)
			if len(choices) > 0 {
				choice, _ := choices[0].(map[string]any)
				reason, _ := choice["finish_reason"].(string)
				complete = reason == "stop"
			}
		case "openai-responses":
			status, _ := body["status"].(string)
			complete = status == "completed"
		case "claude":
			reason, _ := body["stop_reason"].(string)
			complete = reason == "end_turn" || reason == "stop_sequence"
		case "gemini":
			candidates, _ := body["candidates"].([]any)
			if len(candidates) > 0 {
				candidate, _ := candidates[0].(map[string]any)
				complete = candidate["finishReason"] == "STOP"
			}
		}
		return modelTestJSONOutput(body, protocol), complete
	}
	if protocol != "openai-responses" {
		return "", false
	}
	// Codex OAuth requires SSE. Prefer the final completed response, otherwise
	// join only text deltas; reasoning, tool calls, usage, and errors are ignored.
	var deltas strings.Builder
	finalText := ""
	complete := false
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if event["type"] == "response.completed" {
			response, _ := event["response"].(map[string]any)
			if text := modelTestJSONOutput(response, protocol); text != "" {
				finalText = text
			}
			status, _ := response["status"].(string)
			complete = status == "" || status == "completed"
		}
		if event["type"] == "response.failed" || event["type"] == "response.incomplete" || event["type"] == "error" {
			complete = false
		}
		if event["type"] == "response.output_text.delta" {
			text, _ := event["delta"].(string)
			deltas.WriteString(text)
		}
	}
	if finalText != "" {
		return finalText, complete
	}
	return deltas.String(), complete
}
