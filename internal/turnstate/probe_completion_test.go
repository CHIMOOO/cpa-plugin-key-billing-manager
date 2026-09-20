package turnstate

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const completedProbeSSE = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":null,\"incomplete_details\":null}}\n\n"

func TestStrictProbeCompletionFormats(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
	}{
		{"sse", "text/event-stream", completedProbeSSE, ""},
		{"sse-crlf", "text/event-stream; charset=utf-8", strings.ReplaceAll(completedProbeSSE, "\n", "\r\n"), ""},
		{"sse-cr", "text/event-stream", strings.ReplaceAll(completedProbeSSE, "\n", "\r"), ""},
		{"sse-bom-and-heartbeat", "text/event-stream", "\uFEFF: keepalive\n\nid: 1\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" + completedProbeSSE, ""},
		{"sse-event-name", "text/event-stream", "event: response.completed\ndata: {\"response\":{\"status\":\"completed\"}}\n\n", ""},
		{"sse-multiline", "text/event-stream", "data: {\"type\":\"response.completed\",\ndata: \"response\":{\"status\":\"completed\"}}\n\n", ""},
		{"sse-truncated-event", "text/event-stream", strings.TrimSuffix(completedProbeSSE, "\n"), probeCompletionMissing},
		{"sse-truncated-line", "text/event-stream", strings.TrimSpace(completedProbeSSE), probeCompletionMissing},
		{"sse-done-only", "text/event-stream", "data: [DONE]\n\n", probeCompletionMissing},
		{"sse-text-only", "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n", probeCompletionMissing},
		{"sse-failed-before-completed", "text/event-stream", "data: {\"type\":\"response.failed\"}\n\n" + completedProbeSSE, probeCompletionFailed},
		{"sse-failed-named-event", "text/event-stream", "event: error\n\n", probeCompletionFailed},
		{"sse-completed-with-error", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":{\"message\":\"secret upstream body\"}}}\n\n", probeCompletionFailed},
		{"sse-completed-with-incomplete-details", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"incomplete_details\":{}}}\n\n", probeCompletionFailed},
		{"sse-contradictory-event", "text/event-stream", "event: response.output_text.delta\n" + completedProbeSSE, probeCompletionMalformed},
		{"sse-terminal-still-in-progress", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"in_progress\"}}\n\n", probeCompletionMissing},
		{"sse-malformed-json", "text/event-stream", "data: broken\n\n" + completedProbeSSE, probeCompletionMalformed},
		{"sse-null-json", "text/event-stream", "event: response.completed\ndata: null\n\n", probeCompletionMalformed},
		{"json", "application/json", `{"status":"completed","error":null,"incomplete_details":null}`, ""},
		{"json-suffix", "application/responses+json", `{"status":"completed"}`, ""},
		{"json-failed", "application/json", `{"status":"failed"}`, probeCompletionFailed},
		{"json-incomplete", "application/json", `{"status":"incomplete"}`, probeCompletionFailed},
		{"json-queued", "application/json", `{"status":"queued"}`, probeCompletionMissing},
		{"json-error", "application/json", `{"status":"completed","error":{"message":"secret upstream body"}}`, probeCompletionFailed},
		{"json-truncated", "application/json", `{"status":"completed"`, probeCompletionMalformed},
		{"json-concatenated", "application/json", `{"status":"completed"}{"status":"failed"}`, probeCompletionMalformed},
		{"html", "text/html", "<html>OK</html>", probeCompletionFormat},
		{"missing-content-type", "", completedProbeSSE, probeCompletionFormat},
		{"empty-body", "text/event-stream", "", probeCompletionMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{Header: http.Header{"Content-Type": {tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			if got := verifyProbeCompletion(response); got != tc.want {
				t.Fatalf("completion failure = %q, want %q", got, tc.want)
			}
		})
	}
}

type completionTestBody struct {
	reader        io.Reader
	reads, closed int
}

func (b *completionTestBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func (b *completionTestBody) Close() error {
	b.closed++
	return nil
}

type completionTestTransport func(*http.Request) (*http.Response, error)

func (f completionTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type completionFailureReader struct{}

func (completionFailureReader) Read([]byte) (int, error) {
	return 0, errors.New("secret transport body detail")
}

func TestStrictProbeBoundsAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		strict                bool
		status                int
		contentType, encoding string
		reader                io.Reader
		want                  string
		wantRead              bool
	}{
		{"legacy-no-read", false, 200, "text/event-stream", "", completionFailureReader{}, "", false},
		{"non-200-no-read", true, 429, "text/event-stream", "", completionFailureReader{}, "", false},
		{"proxy-auth-no-read", true, 407, "text/event-stream", "", completionFailureReader{}, "", false},
		{"strict-success-close", true, 200, "text/event-stream", "", strings.NewReader(completedProbeSSE), "", true},
		{"strict-read-error", true, 200, "text/event-stream", "", completionFailureReader{}, probeCompletionReadFailed, true},
		{"strict-json-read-error", true, 200, "application/json", "", completionFailureReader{}, probeCompletionReadFailed, true},
		{"strict-zstd", true, 200, "text/event-stream", "zstd", completionFailureReader{}, probeCompletionEncoding, false},
		{"strict-sse-limit", true, 200, "text/event-stream", "", strings.NewReader(":" + strings.Repeat("x", probeCompletionLimit) + "\n\n" + completedProbeSSE), probeCompletionTooLarge, true},
		{"strict-json-limit", true, 200, "application/json", "", strings.NewReader(`{"status":"completed","padding":"` + strings.Repeat("x", probeCompletionLimit) + `"}`), probeCompletionTooLarge, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &completionTestBody{reader: tc.reader}
			client := &http.Client{Transport: completionTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}, "Content-Encoding": {tc.encoding}, Header: {"state-from-headers"}}, Body: body}, nil
			})}
			credential, _ := dummyCredential("")
			credential.ProbeVerifyCompletion = tc.strict
			got, err := doHTTPProbe(client, "http://probe.invalid", credential, "model-a")
			if err != nil || got.CompletionFailure != tc.want || got.ProxyFailure || got.Status != tc.status || got.Value != "state-from-headers" {
				t.Fatalf("strict response = %+v, %v", got, err)
			}
			if body.closed != 1 || (body.reads != 0) != tc.wantRead {
				t.Fatalf("body closed %d, reads %d", body.closed, body.reads)
			}
		})
	}
}

func TestStrictProbeGzipLimitAppliesAfterDecompression(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "over-limit"}[large], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("transport did not negotiate gzip")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set(Header, "state-from-headers")
				writer := gzip.NewWriter(w)
				if large {
					_, _ = io.WriteString(writer, ":"+strings.Repeat("x", probeCompletionLimit)+"\n\n")
				}
				_, _ = io.WriteString(writer, completedProbeSSE)
				_ = writer.Close()
			}))
			defer server.Close()
			credential, _ := dummyCredential("")
			credential.ProbeVerifyCompletion = true
			got, err := doHTTPProbe(probeTestClient(t, ""), server.URL, credential, "model-a")
			want := ""
			if large {
				want = probeCompletionTooLarge
			}
			if err != nil || got.CompletionFailure != want || got.Status != 200 {
				t.Fatalf("gzip response = %+v, %v", got, err)
			}
		})
	}
}

func TestStrictProbeTimeoutAndTerminalEventCloseConnection(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "terminal-event"}[completed], func(t *testing.T) {
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set(Header, "state-from-headers")
				if completed {
					_, _ = io.WriteString(w, completedProbeSSE)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			}))
			defer server.Close()
			client := probeTestClient(t, "")
			client.Timeout = 100 * time.Millisecond
			credential, _ := dummyCredential("")
			credential.ProbeVerifyCompletion = true
			start := time.Now()
			got, err := doHTTPProbe(client, server.URL, credential, "model-a")
			want := probeCompletionReadFailed
			if completed {
				want = ""
			}
			if err != nil || got.CompletionFailure != want || got.ProxyFailure || got.Status != 200 {
				t.Fatalf("strict stream response = %+v, %v", got, err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("strict body read exceeded total timeout")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("strict probe left connection running after return")
			}
		})
	}
}

func TestStrictProbeFailurePreservesTemplateAndDoesNotDiscardProxy(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "template-length", true: "degraded-length"}[degraded], func(t *testing.T) {
			m, now, old := continuityManager(t)
			configurePruning(t, m, map[string]any{"probe_verify_completion": true, "probe_proxies": []string{pruningProxyA, pruningProxyB}, "probe_drop_failed_proxies": true, "probe_drop_degraded_proxies": true, "probe_min_proxies": 1})
			if !readRestarted(t, m).Status().Config.ProbeVerifyCompletion {
				t.Fatal("strict mode configuration was not persisted")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				value := tokenAt(*now)
				if degraded {
					value = strings.Repeat("x", 312)
				}
				w.Header().Set(Header, value)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"error\":{\"message\":\"secret body detail\"}}\n\n")
			}))
			defer server.Close()
			m.runProbe = func(credential Credential, model, proxy string) (ProbeResponse, error) {
				if !credential.ProbeVerifyCompletion {
					t.Fatal("strict setting was not passed to probe transport")
				}
				return doHTTPProbe(probeTestClient(t, ""), server.URL, credential, model)
			}
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.Action != "error" || result.Reason != probeCompletionFailed || result.ProxyDisposition != "" {
				t.Fatalf("strict renewal = %+v, %v", result, err)
			}
			if m.Status().ProxyCounts["static"] != 2 {
				t.Fatal("strict response failure discarded an unproven proxy")
			}
			assertBusinessTemplate(t, m, "account-a", "model-a", old)
			assertBusinessTemplate(t, readRestarted(t, m), "account-a", "model-a", old)
			if rows := m.Status().Templates; len(rows) != 1 || !rows[0].ExpiresAt.Equal(now.Add(10*time.Minute)) {
				t.Fatal("failed strict renewal altered original expiry")
			}
		})
	}
}

func TestStrictProbeDefaultOffAndSuccessfulHarvest(t *testing.T) {
	m, now := newTestManager(t)
	if m.Status().Config.ProbeVerifyCompletion {
		t.Fatal("strict mode must be opt-in")
	}
	if err := m.Update([]byte(`{"probe_verify_completion":true}`)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(Header, tokenAt(*now))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedProbeSSE)
	}))
	defer server.Close()
	m.runProbe = func(credential Credential, model, proxy string) (ProbeResponse, error) {
		return doHTTPProbe(probeTestClient(t, ""), server.URL, credential, model)
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "harvested" || len(m.Status().Templates) != 1 {
		t.Fatalf("strict harvest = %+v, %v", result, err)
	}
}
