package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func armedCapture(t *testing.T) (*trafficCapture, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	c := newTrafficCapture()
	c.now = func() time.Time { return now }
	c.authID, c.authIndex, c.label = "dummy-auth", "dummy-index", "dummy@example.invalid"
	c.expires = now.Add(captureLease)
	c.armed.Store(true)
	return c, &now
}

func captureRequest(id, auth string) RequestInterceptRequest {
	return RequestInterceptRequest{
		RequestID:    id,
		SourceFormat: "openai-response",
		ToFormat:     "codex",
		Model:        "dummy-model",
		Stream:       true,
		Headers:      http.Header{"Authorization": {"Bearer dummy-downstream-key"}, "X-Api-Key": {"dummy-key"}, "Content-Type": {"application/json"}},
		Body:         []byte(`{"model":"dummy-model","input":"hello"}`),
		Metadata:     map[string]any{MetadataSelectedAuth: auth, MetadataSelectedIndex: "dummy-index", MetadataRequestPath: "/v1/responses"},
	}
}

func captureChunk(t *testing.T, id string, index int, body string) []byte {
	t.Helper()
	return mustMarshal(t, map[string]any{
		"RequestID":       id,
		"ChunkIndex":      index,
		"Body":            []byte(body),
		"RequestBody":     []byte(`{"upstream":true}`),
		"ResponseHeaders": http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"dummy=1"}},
		"Metadata":        map[string]any{MetadataSelectedAuth: "dummy-auth"},
	})
}

func TestTrafficCaptureIsInertUntilArmed(t *testing.T) {
	c := newTrafficCapture()
	c.observeRequest(captureRequest("req-1", "dummy-auth"), RequestInterceptResponse{})
	c.observeResponse(captureChunk(t, "req-1", 0, "data: x\n\n"), true)
	c.observeCompletion(RequestCompletion{RequestID: "req-1", Outcome: "succeeded", StatusCode: 200})
	if len(c.entries) != 0 || c.revision != 0 {
		t.Fatalf("an unarmed capture recorded traffic: %+v", c.entries)
	}
}

func TestTrafficCaptureRecordsTheWatchedAccountWithRedactedHeaders(t *testing.T) {
	c, _ := armedCapture(t)
	c.observeRequest(captureRequest("other", "another-auth"), RequestInterceptResponse{})
	c.observeRequest(captureRequest("req-1", "dummy-auth"), RequestInterceptResponse{Headers: http.Header{"X-Codex-Turn-State": {"dummy-state"}}})
	if len(c.entries) != 1 || c.entries[0].RequestID != "req-1" {
		t.Fatalf("entries = %+v", c.entries)
	}
	c.observeResponse(captureChunk(t, "req-1", -1, ""), true)
	c.observeResponse(captureChunk(t, "req-1", 0, "data: one\n\n"), true)
	c.observeResponse(captureChunk(t, "req-1", 1, "data: two\n\n"), true)
	c.observeCompletion(RequestCompletion{RequestID: "req-1", Outcome: "succeeded", StatusCode: 200})

	entry := c.entries[0]
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"dummy-downstream-key", "dummy-key\"", "dummy=1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("captured entry leaked %q: %s", secret, text)
		}
	}
	if entry.RequestHeaders.Get("Authorization") != "Bearer "+captureRedacted || entry.RequestHeaders.Get("X-Codex-Turn-State") != "dummy-state" {
		t.Fatalf("request headers = %v", entry.RequestHeaders)
	}
	if string(entry.ResponseBody.data) != "data: one\n\ndata: two\n\n" || entry.Chunks != 2 || entry.StatusCode != 200 || entry.CompletedAt == nil {
		t.Fatalf("response = %q chunks=%d status=%d", entry.ResponseBody.data, entry.Chunks, entry.StatusCode)
	}
	if string(entry.UpstreamBody.data) != `{"upstream":true}` || entry.Path != "/v1/responses" || !entry.Stream {
		t.Fatalf("entry = %+v", entry)
	}
	if c.pending.Load() != 0 {
		t.Fatal("a completed request must leave the in-flight set")
	}
}

func TestTrafficCaptureFollowsRetriesAndExpires(t *testing.T) {
	c, now := armedCapture(t)
	c.observeRequest(captureRequest("req-1", "dummy-auth"), RequestInterceptResponse{})
	c.observeRequest(captureRequest("req-1", "another-auth"), RequestInterceptResponse{})
	c.observeResponse(captureChunk(t, "req-1", 0, "data: elsewhere\n\n"), true)
	if !c.entries[0].MovedAway || len(c.entries[0].ResponseBody.data) != 0 {
		t.Fatalf("a retry on another account must not attach its response: %+v", c.entries[0])
	}

	*now = now.Add(captureLease + time.Second)
	c.observeRequest(captureRequest("req-2", "dummy-auth"), RequestInterceptResponse{})
	if len(c.entries) != 1 || c.armed.Load() {
		t.Fatalf("an expired listener kept capturing: %+v", c.entries)
	}
}

func TestTrafficCaptureBoundsBodiesAndEntries(t *testing.T) {
	c, _ := armedCapture(t)
	large := captureRequest("req-large", "dummy-auth")
	large.Body = []byte(strings.Repeat("é", captureMaxBodyBytes))
	c.observeRequest(large, RequestInterceptResponse{})
	body := c.entries[0].RequestBody
	if !body.Truncated || len(body.data) > captureMaxBodyBytes || body.Size != len(large.Body) {
		t.Fatalf("body truncated=%v kept=%d size=%d", body.Truncated, len(body.data), body.Size)
	}
	for i := range captureMaxEntries + 5 {
		c.observeRequest(captureRequest("req-"+string(rune('a'+i%26))+strings.Repeat("x", i), "dummy-auth"), RequestInterceptResponse{})
	}
	if len(c.entries) != captureMaxEntries || c.total > captureMaxTotal {
		t.Fatalf("entries=%d total=%d", len(c.entries), c.total)
	}
}

func TestTrafficCaptureSettingRegistersResponseHooksWithoutState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.db")
	configYAML := []byte("enabled: true\nstate_file: \"" + state + "\"\n")
	register := func(app *App) Registration {
		t.Helper()
		raw, err := app.HandleMethod(MethodPluginRegister, mustMarshal(t, LifecycleRequest{ConfigYAML: configYAML}))
		if err != nil {
			t.Fatal(err)
		}
		var reg Registration
		decodeResult(t, raw, &reg)
		return reg
	}
	// A fresh App stands in for a CPA restart that reloads persisted settings.
	restart := func() *App {
		app := newTestApp(t)
		t.Cleanup(app.Shutdown)
		return app
	}
	app := restart()
	register(app)
	if err := app.turnState.Update([]byte(`{"suspended":true}`)); err != nil {
		t.Fatal(err)
	}
	app = restart()
	if caps := register(app).Capabilities; caps.ResponseInterceptor || caps.StreamChunkInterceptor {
		t.Fatalf("response hooks registered without State or capture: %+v", caps)
	}
	response := callManagement(t, app, http.MethodPut, "/traffic-capture/settings", nil, map[string]bool{"response_hooks": true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("settings status = %d body=%s", response.StatusCode, response.Body)
	}
	app = restart()
	caps := register(app).Capabilities
	if !caps.ResponseInterceptor || !caps.StreamChunkInterceptor || caps.ModelRouter {
		t.Fatalf("capture must register only the response hooks: %+v", caps)
	}
	status := callManagement(t, app, http.MethodGet, "/traffic-capture", url.Values{}, nil)
	var view struct {
		ResponseHooks struct{ Wanted, Registered bool } `json:"response_hooks"`
	}
	if json.Unmarshal(status.Body, &view) != nil || !view.ResponseHooks.Wanted || !view.ResponseHooks.Registered {
		t.Fatalf("status = %s", status.Body)
	}
}
