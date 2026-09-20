package turnstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	probeCompletionLimit      = 1 << 20
	probeCompletionTooLarge   = "Strict probe verification failed: the response body exceeded 1 MiB; no template was saved"
	probeCompletionReadFailed = "Strict probe verification failed: the response body could not be read completely within 25 seconds; no template was saved"
	probeCompletionEncoding   = "Strict probe verification failed: unsupported response content encoding; no template was saved"
	probeCompletionFormat     = "Strict probe verification failed: expected an SSE or JSON response; no template was saved"
	probeCompletionMalformed  = "Strict probe verification failed: malformed response data; no template was saved"
	probeCompletionFailed     = "Strict probe verification failed: the upstream response failed or was incomplete; no template was saved"
	probeCompletionMissing    = "Strict probe verification failed: no completed response was confirmed; no template was saved"
)

// verifyProbeCompletion is used only by synthetic active probes. Diagnostics
// are fixed strings: neither upstream response text nor credentials escape to
// management events. Transport automatically decompresses gzip; the limit is
// applied to decoded bytes. Unknown encodings are rejected rather than guessed.
func verifyProbeCompletion(response *http.Response) string {
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return probeCompletionEncoding
	}
	kind, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		return probeCompletionFormat
	}
	reader := &io.LimitedReader{R: response.Body, N: probeCompletionLimit + 1}
	switch {
	case strings.EqualFold(kind, "text/event-stream"):
		return verifyProbeSSECompletion(reader)
	case strings.EqualFold(kind, "application/json"), strings.HasPrefix(strings.ToLower(kind), "application/") && strings.HasSuffix(strings.ToLower(kind), "+json"):
		raw, err := io.ReadAll(reader)
		if reader.N == 0 {
			return probeCompletionTooLarge
		}
		if err != nil {
			return probeCompletionReadFailed
		}
		var envelope probeCompletionEnvelope
		if !decodeProbeEnvelope(raw, &envelope) {
			return probeCompletionMalformed
		}
		if envelope.unsuccessful() {
			return probeCompletionFailed
		}
		if envelope.Status != "completed" {
			return probeCompletionMissing
		}
		return ""
	default:
		return probeCompletionFormat
	}
}

type probeCompletionEnvelope struct {
	Type              string                   `json:"type"`
	Status            string                   `json:"status"`
	Error             json.RawMessage          `json:"error"`
	IncompleteDetails json.RawMessage          `json:"incomplete_details"`
	Response          *probeCompletionEnvelope `json:"response"`
}

func decodeProbeEnvelope(raw []byte, envelope *probeCompletionEnvelope) bool {
	raw = bytes.TrimSpace(raw)
	// A JSON null decodes into a zero struct without an error; it must not
	// become a valid response just because an SSE event name says completed.
	return len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, envelope) == nil
}

func probeHasValue(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) != 0 && !bytes.Equal(value, []byte("null"))
}

func probeFailureType(kind string) bool {
	switch kind {
	case "error", "response.failed", "response.incomplete", "response.cancelled":
		return true
	}
	return false
}

func (e probeCompletionEnvelope) unsuccessful() bool {
	if probeFailureType(e.Type) || probeHasValue(e.Error) || probeHasValue(e.IncompleteDetails) {
		return true
	}
	switch e.Status {
	case "failed", "incomplete", "cancelled":
		return true
	}
	return e.Response != nil && e.Response.unsuccessful()
}

// SSE completion must be a complete dispatched event (terminated by a blank
// line), with an explicit response.completed type. [DONE], output text, and
// clean EOF alone are not completion evidence. A completed event is terminal,
// so we close there rather than waiting indefinitely for an SSE socket to end.
func verifyProbeSSECompletion(reader *io.LimitedReader) string {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), probeCompletionLimit+2)
	scanner.Split(probeSSELine)
	var event string
	var data strings.Builder
	firstLine := true
	for scanner.Scan() {
		if reader.N == 0 {
			return probeCompletionTooLarge
		}
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\uFEFF")
			firstLine = false
		}
		if line == "" {
			if probeFailureType(event) {
				return probeCompletionFailed
			}
			if data.Len() != 0 {
				raw := strings.TrimSpace(data.String())
				if raw == "[DONE]" {
					return probeCompletionMissing
				}
				var envelope probeCompletionEnvelope
				if !decodeProbeEnvelope([]byte(raw), &envelope) {
					return probeCompletionMalformed
				}
				if envelope.unsuccessful() {
					return probeCompletionFailed
				}
				if event == "response.completed" || envelope.Type == "response.completed" {
					// Reject contradictory event/data types and terminal status.
					if event != "" && event != "message" && event != "response.completed" || envelope.Type != "" && envelope.Type != "response.completed" {
						return probeCompletionMalformed
					}
					if envelope.Status != "" && envelope.Status != "completed" || envelope.Response != nil && envelope.Response.Status != "" && envelope.Response.Status != "completed" {
						return probeCompletionMissing
					}
					return ""
				}
			}
			event = ""
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if reader.N == 0 {
		return probeCompletionTooLarge
	}
	if scanner.Err() != nil {
		return probeCompletionReadFailed
	}
	return probeCompletionMissing
}

// EventSource permits LF, CRLF and CR line endings. Do not dispatch a final
// unterminated line at EOF: a truncated completion event is not confirmation.
func probeSSELine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
	}
	return 0, nil, nil
}
