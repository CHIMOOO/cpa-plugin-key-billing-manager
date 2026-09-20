package plugin

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This is metadata from an explicitly submitted diagnostic response only.
// It never contributes to billing, usage, routing or business failure records.
func modelTestDeclaredModel(raw, protocol, upstream string) (string, string) {
	observed := ""
	conflicting := false
	observe := func(value any) {
		name, ok := value.(string)
		if !ok || name == "" || len(name) > 256 || !utf8.ValidString(name) ||
			strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return
		}
		if observed != "" && observed != name {
			conflicting = true
		}
		observed = name
	}
	var body map[string]any
	if json.Unmarshal([]byte(raw), &body) == nil {
		switch protocol {
		case "openai", "openai-responses", "claude":
			observe(body["model"])
		case "gemini":
			observe(body["modelVersion"])
		}
	} else if protocol == "openai-responses" {
		// SSE data can span several lines. Only lifecycle response objects carry
		// the declaration; model names inside text/tool deltas are not metadata.
		var data strings.Builder
		consume := func() {
			defer data.Reset()
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Model any `json:"model"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(data.String()), &event) != nil {
				return
			}
			switch event.Type {
			case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete":
				observe(event.Response.Model)
			}
		}
		for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
			if line == "" {
				consume()
			} else if strings.HasPrefix(line, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if data.Len() > 0 {
			consume()
		}
	}
	if conflicting {
		return "", "conflicting"
	}
	if observed == "" {
		return "", "missing"
	}
	if observed == upstream {
		return observed, "matched"
	}
	return observed, "different"
}
