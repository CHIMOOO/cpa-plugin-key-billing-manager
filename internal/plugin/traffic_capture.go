package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"cpa-key-billing/internal/billing"
)

// Traffic capture is an operator debugging view. It records what this plugin's
// hooks observe for the selected upstream accounts, only in memory and only
// while the management page keeps polling. It never feeds billing, usage or
// routing: usage.handle remains the only source of those values.
const (
	captureMaxEntries   = 100
	captureMaxBodyBytes = 2 << 20
	captureMaxTotal     = 48 << 20
	captureLease        = 45 * time.Second
	captureMaxListeners = 64
	captureRedacted     = "[redacted]"
)

type captureSettings struct {
	ResponseHooks bool `json:"response_hooks"`
}

// captureBody keeps bytes so streamed chunks append in amortized time; the
// text is materialized only when a detail view is serialized.
type captureBody struct {
	data      []byte
	Size      int
	Truncated bool
	Binary    bool
}

func (b captureBody) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Text      string `json:"text,omitempty"`
		Size      int    `json:"size"`
		Truncated bool   `json:"truncated,omitempty"`
		Binary    bool   `json:"binary,omitempty"`
	}{string(trimUTF8Tail(b.data)), b.Size, b.Truncated, b.Binary})
}

type captureEntry struct {
	Seq             uint64      `json:"seq"`
	Revision        uint64      `json:"revision"`
	RequestID       string      `json:"request_id"`
	Attempts        int         `json:"attempts"`
	AuthID          string      `json:"auth_id"`
	AuthIndex       string      `json:"auth_index,omitempty"`
	AuthName        string      `json:"auth_name,omitempty"`
	Path            string      `json:"path,omitempty"`
	SourceFormat    string      `json:"source_format,omitempty"`
	ToFormat        string      `json:"to_format,omitempty"`
	Model           string      `json:"model,omitempty"`
	RequestedModel  string      `json:"requested_model,omitempty"`
	ResponseModel   string      `json:"response_model,omitempty"`
	Stream          bool        `json:"stream"`
	StartedAt       time.Time   `json:"started_at"`
	CompletedAt     *time.Time  `json:"completed_at,omitempty"`
	RequestHeaders  http.Header `json:"request_headers,omitempty"`
	RequestBody     captureBody `json:"request_body"`
	UpstreamBody    captureBody `json:"upstream_body"`
	Rejected        bool        `json:"rejected,omitempty"`
	StatusCode      int         `json:"status_code,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
	ResponseBody    captureBody `json:"response_body"`
	Responded       bool        `json:"responded"`
	Chunks          int         `json:"chunks,omitempty"`
	Outcome         string      `json:"outcome,omitempty"`
	Error           string      `json:"error,omitempty"`
	MovedAway       bool        `json:"moved_away,omitempty"`

	bytes int
}

// captureSummary is the polled list row; bodies are fetched on demand.
type captureSummary struct {
	Seq            uint64     `json:"seq"`
	Revision       uint64     `json:"revision"`
	RequestID      string     `json:"request_id"`
	Attempts       int        `json:"attempts"`
	AuthIndex      string     `json:"auth_index,omitempty"`
	AuthName       string     `json:"auth_name,omitempty"`
	Model          string     `json:"model,omitempty"`
	RequestedModel string     `json:"requested_model,omitempty"`
	ResponseModel  string     `json:"response_model,omitempty"`
	Stream         bool       `json:"stream"`
	StartedAt      time.Time  `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	StatusCode     int        `json:"status_code,omitempty"`
	Outcome        string     `json:"outcome,omitempty"`
	Rejected       bool       `json:"rejected,omitempty"`
	Responded      bool       `json:"responded"`
	MovedAway      bool       `json:"moved_away,omitempty"`
	RequestSize    int        `json:"request_size"`
	ResponseSize   int        `json:"response_size"`
	Chunks         int        `json:"chunks,omitempty"`
}

func (e *captureEntry) summary() captureSummary {
	return captureSummary{
		Seq:            e.Seq,
		Revision:       e.Revision,
		RequestID:      e.RequestID,
		Attempts:       e.Attempts,
		AuthIndex:      e.AuthIndex,
		AuthName:       e.AuthName,
		Model:          e.Model,
		RequestedModel: e.RequestedModel,
		ResponseModel:  e.ResponseModel,
		Stream:         e.Stream,
		StartedAt:      e.StartedAt,
		CompletedAt:    e.CompletedAt,
		StatusCode:     e.StatusCode,
		Outcome:        e.Outcome,
		Rejected:       e.Rejected,
		Responded:      e.Responded,
		MovedAway:      e.MovedAway,
		RequestSize:    e.RequestBody.Size,
		ResponseSize:   e.ResponseBody.Size,
		Chunks:         e.Chunks,
	}
}

// captureListener is one watched account; a paused listener keeps its place
// in the list but records nothing new.
type captureListener struct {
	authID    string
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Paused    bool   `json:"paused"`
}

type trafficCapture struct {
	mu        sync.Mutex
	now       func() time.Time
	path      string
	settings  captureSettings
	listeners []*captureListener
	expires   time.Time
	seq       uint64
	revision  uint64
	total     int
	entries   []*captureEntry
	inflight  map[string]*captureEntry

	// armed and pending let hooks skip decoding while nothing is captured.
	armed   atomic.Bool
	pending atomic.Int32
}

func newTrafficCapture() *trafficCapture {
	return &trafficCapture{now: time.Now, inflight: map[string]*captureEntry{}}
}

func loadCaptureSettings(path string) (captureSettings, error) {
	var settings captureSettings
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return settings, errors.New("Cannot read traffic capture settings")
	}
	if len(raw) > 4096 || decodeStrict(raw, &settings) != nil {
		return settings, errors.New("Invalid traffic capture settings")
	}
	return settings, nil
}

func (c *trafficCapture) install(path string, settings captureSettings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path, c.settings = path, settings
}

func (c *trafficCapture) responseHooksWanted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings.ResponseHooks
}

// activeLocked reports whether any listener is still leased. Expiry is lazy:
// the plugin runs no timers, so a closed page stops capture at the next request.
func (c *trafficCapture) activeLocked(now time.Time) bool {
	if len(c.listeners) == 0 {
		return false
	}
	if now.After(c.expires) {
		c.listeners = nil
		c.armed.Store(false)
		return false
	}
	return true
}

func (c *trafficCapture) rearmLocked() {
	armed := false
	for _, listener := range c.listeners {
		armed = armed || !listener.Paused
	}
	c.armed.Store(armed)
}

func (c *trafficCapture) listenerLocked(authIndex string) *captureListener {
	for _, listener := range c.listeners {
		if listener.AuthIndex == authIndex {
			return listener
		}
	}
	return nil
}

func (c *trafficCapture) matchLocked(metadata map[string]any) *captureListener {
	id, index := metadataString(metadata, MetadataSelectedAuth), metadataString(metadata, MetadataSelectedIndex)
	for _, listener := range c.listeners {
		if listener.Paused {
			continue
		}
		if listener.authID != "" && id == listener.authID || index != "" && index == listener.AuthIndex {
			return listener
		}
	}
	return nil
}

func captureModelName(value any) string {
	name, ok := value.(string)
	if !ok || name == "" || len(name) > 256 || !utf8.ValidString(name) ||
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	return name
}

type captureModelField struct {
	Model any `json:"model"`
}

func captureDeclaredModel(data []byte) string {
	var body struct {
		Model        any               `json:"model"`
		ModelVersion any               `json:"modelVersion"`
		Response     captureModelField `json:"response"`
		Message      captureModelField `json:"message"`
	}
	// A type mismatch in one field still fills the others.
	var syntax *json.SyntaxError
	if err := json.Unmarshal(data, &body); errors.As(err, &syntax) {
		return ""
	}
	for _, value := range []any{body.Response.Model, body.Message.Model, body.Model, body.ModelVersion} {
		if name := captureModelName(value); name != "" {
			return name
		}
	}
	return ""
}

// captureResponseModel reads the model a provider declares in a response body
// or in the SSE events of one stream chunk. A model name inside generated text
// is an escaped JSON string, so it never reads as a top-level key.
func captureResponseModel(raw []byte) string {
	if !bytes.Contains(raw, []byte(`"model`)) {
		return ""
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		return captureDeclaredModel(trimmed)
	}
	model := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		if name := captureDeclaredModel(bytes.TrimSpace(data)); name != "" {
			model = name
		}
	}
	return model
}

func captureBodyOf(raw []byte) captureBody {
	body := captureBody{Size: len(raw)}
	if len(raw) == 0 {
		return body
	}
	cut := raw
	if len(cut) > captureMaxBodyBytes {
		cut, body.Truncated = trimUTF8Tail(cut[:captureMaxBodyBytes]), true
	}
	if !utf8.Valid(cut) {
		body.Binary = true
		return body
	}
	body.data = append([]byte(nil), cut...)
	return body
}

// appendCaptureBody does not validate each chunk: a rune may be split across
// stream chunks, and JSON encoding replaces any invalid byte when displayed.
func appendCaptureBody(body *captureBody, chunk []byte) int {
	body.Size += len(chunk)
	if body.Truncated || body.Binary {
		return 0
	}
	room := captureMaxBodyBytes - len(body.data)
	if len(chunk) > room {
		body.Truncated = true
		chunk = chunk[:max(room, 0)]
	}
	body.data = append(body.data, chunk...)
	return len(chunk)
}

// trimUTF8Tail drops a rune split by a byte limit, never more than one rune.
func trimUTF8Tail(raw []byte) []byte {
	for cut := 0; cut < utf8.UTFMax && cut < len(raw); cut++ {
		if utf8.Valid(raw[:len(raw)-cut]) {
			return raw[:len(raw)-cut]
		}
	}
	return raw
}

var captureSecretParts = []string{"authorization", "cookie", "api-key", "apikey", "token", "secret", "password", "signature", "x-goog-api-key"}

// redactHeaders keeps the header shape but never the value of anything that
// may carry a downstream or upstream credential.
func redactHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	result := make(http.Header, len(headers))
	for name, values := range headers {
		lower := strings.ToLower(name)
		secret := false
		for _, part := range captureSecretParts {
			if strings.Contains(lower, part) {
				secret = true
				break
			}
		}
		if lower == "x-api-key" || strings.HasSuffix(lower, "-key") {
			secret = true
		}
		copied := make([]string, len(values))
		for i, value := range values {
			if !secret {
				copied[i] = value
				continue
			}
			if scheme, _, found := strings.Cut(strings.TrimSpace(value), " "); found && len(scheme) <= 16 && !strings.ContainsAny(scheme, "=:") {
				copied[i] = scheme + " " + captureRedacted
			} else {
				copied[i] = captureRedacted
			}
		}
		result[name] = copied
	}
	return result
}

func mergeCaptureHeaders(current, updates http.Header, clear []string) http.Header {
	merged := current.Clone()
	if merged == nil {
		merged = http.Header{}
	}
	for _, name := range clear {
		merged.Del(name)
	}
	for name, values := range updates {
		merged[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	return merged
}

func (c *trafficCapture) touchLocked(entry *captureEntry, delta int) {
	c.revision++
	entry.Revision = c.revision
	entry.bytes += delta
	c.total += delta
	for len(c.entries) > 1 && (len(c.entries) > captureMaxEntries || c.total > captureMaxTotal) {
		evicted := c.entries[0]
		c.entries = c.entries[1:]
		c.total -= evicted.bytes
		if c.inflight[evicted.RequestID] == evicted {
			delete(c.inflight, evicted.RequestID)
		}
	}
	c.pending.Store(int32(len(c.inflight)))
}

// observeRequest records the request after this plugin's own post-auth
// decision, so the headers are those CPA forwards to the executor.
func (c *trafficCapture) observeRequest(req RequestInterceptRequest, response RequestInterceptResponse) {
	if c == nil || !c.armed.Load() && c.pending.Load() == 0 {
		return
	}
	if metadataString(req.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	requestID := strings.TrimSpace(req.RequestID)
	entry := c.inflight[requestID]
	if entry != nil && metadataString(req.Metadata, MetadataSelectedAuth) != entry.AuthID {
		// A retry that leaves the account takes its response along; when the
		// new account is watched too, the retry starts an entry of its own.
		entry.MovedAway = true
		delete(c.inflight, requestID)
		c.touchLocked(entry, 0)
		entry = nil
	}
	name := ""
	if entry == nil {
		if !c.activeLocked(now) {
			return
		}
		listener := c.matchLocked(req.Metadata)
		if listener == nil {
			return
		}
		name = listener.Name
		c.seq++
		entry = &captureEntry{Seq: c.seq, RequestID: requestID}
		c.entries = append(c.entries, entry)
		if requestID != "" {
			c.inflight[requestID] = entry
		}
	}
	previous := entry.bytes
	if name == "" {
		name = entry.AuthName
	}
	*entry = captureEntry{
		Seq:            entry.Seq,
		RequestID:      requestID,
		Attempts:       entry.Attempts + 1,
		StartedAt:      now,
		AuthID:         metadataString(req.Metadata, MetadataSelectedAuth),
		AuthIndex:      metadataString(req.Metadata, MetadataSelectedIndex),
		AuthName:       name,
		Path:           metadataString(req.Metadata, MetadataRequestPath),
		SourceFormat:   req.SourceFormat,
		ToFormat:       req.ToFormat,
		Model:          req.Model,
		RequestedModel: req.RequestedModel,
		Stream:         req.Stream,
		RequestHeaders: redactHeaders(mergeCaptureHeaders(req.Headers, response.Headers, response.ClearHeaders)),
		RequestBody:    captureBodyOf(req.Body),
	}
	entry.bytes = previous
	delta := len(entry.RequestBody.data) - previous
	if response.Terminate {
		entry.Rejected, entry.Responded, entry.StatusCode = true, true, response.StatusCode
		entry.ResponseHeaders = redactHeaders(response.ResponseHeaders)
		entry.ResponseBody = captureBodyOf(response.ResponseBody)
		delta += len(entry.ResponseBody.data)
	}
	c.touchLocked(entry, delta)
}

type captureResponseRequest struct {
	RequestID       string         `json:"RequestID"`
	Stream          bool           `json:"Stream"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	RequestBody     []byte         `json:"RequestBody"`
	Body            []byte         `json:"Body"`
	StatusCode      int            `json:"StatusCode"`
	ChunkIndex      int            `json:"ChunkIndex"`
	Metadata        map[string]any `json:"Metadata"`
}

func (c *trafficCapture) observeResponse(raw []byte, stream bool) {
	if c == nil || c.pending.Load() == 0 {
		return
	}
	var req captureResponseRequest
	if json.Unmarshal(raw, &req) != nil {
		return
	}
	model := captureResponseModel(req.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.inflight[strings.TrimSpace(req.RequestID)]
	if entry == nil || entry.MovedAway {
		return
	}
	if id := metadataString(req.Metadata, MetadataSelectedAuth); id != "" && entry.AuthID != "" && id != entry.AuthID {
		return
	}
	delta := 0
	if len(req.RequestBody) > 0 && entry.UpstreamBody.Size == 0 {
		entry.UpstreamBody = captureBodyOf(req.RequestBody)
		delta += len(entry.UpstreamBody.data)
	}
	if len(req.ResponseHeaders) > 0 {
		entry.ResponseHeaders = redactHeaders(req.ResponseHeaders)
	}
	entry.Responded = true
	if model != "" {
		entry.ResponseModel = model
	}
	if !stream {
		if req.StatusCode != 0 {
			entry.StatusCode = req.StatusCode
		}
		delta -= len(entry.ResponseBody.data)
		entry.ResponseBody = captureBodyOf(req.Body)
		delta += len(entry.ResponseBody.data)
	} else if req.ChunkIndex >= 0 {
		entry.Chunks++
		delta += appendCaptureBody(&entry.ResponseBody, req.Body)
	}
	c.touchLocked(entry, delta)
}

func (c *trafficCapture) observeCompletion(completion RequestCompletion) {
	if c == nil || c.pending.Load() == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	requestID := strings.TrimSpace(completion.RequestID)
	entry := c.inflight[requestID]
	if entry == nil {
		return
	}
	delete(c.inflight, requestID)
	now := c.now()
	entry.CompletedAt = &now
	entry.Outcome, entry.Error = completion.Outcome, completion.Error
	if completion.StatusCode != 0 {
		entry.StatusCode = completion.StatusCode
	}
	c.touchLocked(entry, 0)
}

type captureAccount struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Disabled  bool   `json:"disabled"`
}

func (a *App) captureAccounts() ([]captureAccount, map[string]hostAuthFile, error) {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return nil, nil, err
	}
	accounts := []captureAccount{}
	byIndex := map[string]hostAuthFile{}
	for _, file := range files {
		if file.AuthIndex == "" || strings.EqualFold(file.Provider, integrationAuthType) || strings.EqualFold(file.Type, integrationAuthType) {
			continue
		}
		view := modelTestAccountView(file)
		// Like the other account lists, hide auth files CPA reports without an email.
		if view.Source == billing.CredentialSourceAuthFiles && cleanText(file.Email) == "" {
			continue
		}
		accounts = append(accounts, captureAccount{AuthIndex: file.AuthIndex, Name: view.Name, Provider: view.Provider, Disabled: view.Disabled})
		byIndex[file.AuthIndex] = file
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return accounts, byIndex, nil
}

func captureJSON(status int, value any) ManagementResponse {
	response := JSONResponse(status, value)
	response.Headers.Set("Cache-Control", "private, no-store")
	response.Headers.Set("Pragma", "no-cache")
	return response
}

// captureStatusLocked renews the lease when asked: polling is the heartbeat.
func (a *App) captureStatusLocked(since uint64, renew bool) map[string]any {
	c := a.capture
	now := c.now()
	active := c.activeLocked(now)
	if active && renew {
		c.expires = now.Add(captureLease)
	}
	rows := []captureSummary{}
	for _, entry := range c.entries {
		if entry.Revision <= since {
			continue
		}
		rows = append(rows, entry.summary())
	}
	seqs := make([]uint64, 0, len(c.entries))
	for _, entry := range c.entries {
		seqs = append(seqs, entry.Seq)
	}
	listeners := []captureListener{}
	for _, listener := range c.listeners {
		listeners = append(listeners, *listener)
	}
	result := map[string]any{
		"listeners":      listeners,
		"revision":       c.revision,
		"entries":        rows,
		"retained":       seqs,
		"response_hooks": map[string]bool{"wanted": c.settings.ResponseHooks, "registered": a.responseHooks.Load(), "state": a.stateHooks.Load()},
		"limits":         map[string]int{"max_entries": captureMaxEntries, "max_body_bytes": captureMaxBodyBytes, "lease_seconds": int(captureLease / time.Second)},
	}
	if active {
		result["expires_at"] = c.expires
	}
	return result
}

func (a *App) getTrafficCapture(req ManagementRequest) ManagementResponse {
	since, _ := strconv.ParseUint(req.Query.Get("since"), 10, 64)
	var accounts []captureAccount
	var accountsErr error
	if req.Query.Get("accounts") == "1" {
		accounts, _, accountsErr = a.captureAccounts()
	}
	a.capture.mu.Lock()
	result := a.captureStatusLocked(since, true)
	a.capture.mu.Unlock()
	if req.Query.Get("accounts") == "1" {
		if accountsErr != nil {
			result["accounts_error"] = "Cannot read the host account inventory"
		} else {
			result["accounts"] = accounts
		}
	}
	return captureJSON(http.StatusOK, result)
}

func (a *App) getTrafficCaptureEntry(req ManagementRequest) ManagementResponse {
	seq, err := strconv.ParseUint(req.Query.Get("seq"), 10, 64)
	if err != nil {
		return JSONError(http.StatusBadRequest, "invalid_capture", "A captured request number is required")
	}
	c := a.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.Seq == seq {
			return captureJSON(http.StatusOK, entry)
		}
	}
	return JSONError(http.StatusNotFound, "capture_not_found", "This captured request is no longer retained")
}

// watchTrafficCapture adds an account to the listeners, or pauses and resumes
// one already listed. Every listener shares the page's polling lease.
func (a *App) watchTrafficCapture(req ManagementRequest) ManagementResponse {
	var input struct {
		AuthIndex string `json:"auth_index"`
		Paused    bool   `json:"paused"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	input.AuthIndex = strings.TrimSpace(input.AuthIndex)
	if input.AuthIndex == "" || len(input.AuthIndex) > 512 {
		return JSONError(http.StatusBadRequest, "invalid_capture", "Select an account to listen to")
	}
	accounts, files, err := a.captureAccounts()
	if err != nil {
		return JSONError(http.StatusBadGateway, "capture_unavailable", "Cannot read the host account inventory")
	}
	file, ok := files[input.AuthIndex]
	if !ok {
		return JSONError(http.StatusBadRequest, "invalid_capture", "The selected account no longer exists")
	}
	label := file.AuthIndex
	for _, account := range accounts {
		if account.AuthIndex == input.AuthIndex {
			label = account.Name
		}
	}
	c := a.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.activeLocked(now)
	listener := c.listenerLocked(file.AuthIndex)
	if listener == nil {
		if len(c.listeners) >= captureMaxListeners {
			return JSONError(http.StatusBadRequest, "invalid_capture", "Too many accounts are being listened to")
		}
		listener = &captureListener{AuthIndex: file.AuthIndex}
		c.listeners = append(c.listeners, listener)
	}
	listener.authID, listener.Name, listener.Paused = file.ID, label, input.Paused
	c.expires = now.Add(captureLease)
	c.rearmLocked()
	return captureJSON(http.StatusOK, a.captureStatusLocked(^uint64(0), false))
}

// stopTrafficCapture removes one listener, or all of them without auth_index.
func (a *App) stopTrafficCapture(req ManagementRequest) ManagementResponse {
	authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
	c := a.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if authIndex == "" {
		c.listeners = nil
	} else {
		kept := c.listeners[:0]
		for _, listener := range c.listeners {
			if listener.AuthIndex != authIndex {
				kept = append(kept, listener)
			}
		}
		clear(c.listeners[len(kept):])
		c.listeners = kept
	}
	c.rearmLocked()
	return captureJSON(http.StatusOK, a.captureStatusLocked(^uint64(0), false))
}

func (a *App) clearTrafficCapture(_ ManagementRequest) ManagementResponse {
	c := a.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries, c.total = nil, 0
	c.inflight = map[string]*captureEntry{}
	c.pending.Store(0)
	c.revision++
	return captureJSON(http.StatusOK, a.captureStatusLocked(^uint64(0), false))
}

func (a *App) setTrafficCaptureSettings(req ManagementRequest) ManagementResponse {
	var input captureSettings
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	c := a.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writePrivateJSON(c.path, input); err != nil {
		return JSONError(http.StatusInternalServerError, "capture_settings_failed", err.Error())
	}
	c.settings = input
	return captureJSON(http.StatusOK, a.captureStatusLocked(^uint64(0), false))
}
