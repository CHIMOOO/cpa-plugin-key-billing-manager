package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/turnstate"
)

const (
	maxRiskStateBytes    = 8 << 20
	maxRiskEventRuleRefs = 16
)

// Local content rules follow the reference project's observe/block workflow.
// Request bodies exist only during inspection; events contain irreversible
// rule and caller references, never prompts, excerpts, headers or credentials.
type riskModelFilter struct {
	Mode   string   `json:"mode"`
	Models []string `json:"models"`
}
type riskConfig struct {
	Enabled         bool            `json:"enabled"`
	Mode            string          `json:"mode"`
	BlockedKeywords []string        `json:"blocked_keywords"`
	ModelFilter     riskModelFilter `json:"model_filter"`
	RememberHashes  bool            `json:"remember_hashes"`
	BlockStatus     int             `json:"block_status"`
	BlockMessage    string          `json:"block_message"`
	RetentionDays   int             `json:"retention_days"`
	MaxEvents       int             `json:"max_events"`
}
type riskEvent struct {
	At        time.Time `json:"at"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason"`
	Model     string    `json:"model"`
	CallerRef string    `json:"caller_ref"`
	RuleRefs  []string  `json:"rule_refs"`
}
type riskDiskState struct {
	Version int         `json:"version"`
	Config  riskConfig  `json:"config"`
	Events  []riskEvent `json:"events"`
	Hashes  []string    `json:"hashes"`
}
type riskControl struct {
	mu           sync.Mutex
	state        riskDiskState
	path         string
	storageError string
	now          func() time.Time
}

func defaultRiskConfig() riskConfig {
	return riskConfig{Mode: "observe", BlockedKeywords: []string{}, ModelFilter: riskModelFilter{Mode: "all", Models: []string{}}, RememberHashes: true, BlockStatus: 403, BlockMessage: "Request blocked by configured risk policy", RetentionDays: 30, MaxEvents: 500}
}
func newRiskControl() *riskControl {
	return &riskControl{state: riskDiskState{Version: 1, Config: defaultRiskConfig(), Events: []riskEvent{}, Hashes: []string{}}, now: time.Now}
}
func validateRiskConfig(c *riskConfig) error {
	if c.Mode != "observe" && c.Mode != "pre_block" {
		return errors.New("Risk mode must be observe or pre_block")
	}
	if c.ModelFilter.Mode != "all" && c.ModelFilter.Mode != "include" && c.ModelFilter.Mode != "exclude" {
		return errors.New("Invalid risk model filter")
	}
	if len(c.BlockedKeywords) > 256 || len(c.ModelFilter.Models) > 128 || c.BlockStatus < 400 || c.BlockStatus > 499 || c.RetentionDays < 1 || c.RetentionDays > 365 || c.MaxEvents < 1 || c.MaxEvents > 2000 {
		return errors.New("Risk policy exceeds its configured limits")
	}
	if len(c.BlockMessage) == 0 || len(c.BlockMessage) > 512 || strings.ContainsAny(c.BlockMessage, "\x00\r\n") {
		return errors.New("Invalid public risk message")
	}
	for _, list := range [][]string{c.BlockedKeywords, c.ModelFilter.Models} {
		for _, s := range list {
			if len(strings.TrimSpace(s)) == 0 || len(s) > 512 || strings.ContainsAny(s, "\x00\r\n") {
				return errors.New("Invalid risk keyword or model")
			}
		}
	}
	return nil
}
func loadRiskControl(path string) (riskDiskState, error) {
	s := newRiskControl().state
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, errors.New("Cannot read content risk settings")
	}
	if len(raw) > maxRiskStateBytes || decodeStrict(raw, &s) != nil || s.Version != 1 || len(s.Events) > 2000 || len(s.Hashes) > 4096 {
		return s, errors.New("Invalid content risk settings")
	}
	if err = validateRiskConfig(&s.Config); err != nil {
		return s, err
	}
	for _, h := range s.Hashes {
		b, e := hex.DecodeString(h)
		if e != nil || len(b) != 32 {
			return s, errors.New("Invalid content risk hash memory")
		}
	}
	return s, nil
}
func (r *riskControl) install(path string, state riskDiskState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if path == r.path {
		return
	}
	r.path, r.state, r.storageError = path, state, ""
}
func (r *riskControl) pruneLocked() {
	cutoff := r.now().Add(-time.Duration(r.state.Config.RetentionDays) * 24 * time.Hour)
	events := []riskEvent{}
	for _, e := range r.state.Events {
		if !e.At.Before(cutoff) {
			events = append(events, e)
		}
	}
	if len(events) > r.state.Config.MaxEvents {
		events = events[len(events)-r.state.Config.MaxEvents:]
	}
	r.state.Events = events
}
func riskDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func riskModelMatches(c riskConfig, model string) bool {
	match := false
	for _, value := range c.ModelFilter.Models {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(model)) {
			match = true
			break
		}
	}
	if !match {
		base := turnstate.ModelName(model)
		for _, value := range c.ModelFilter.Models {
			if strings.EqualFold(turnstate.ModelName(value), base) {
				match = true
				break
			}
		}
	}
	return c.ModelFilter.Mode == "all" || c.ModelFilter.Mode == "include" && match || c.ModelFilter.Mode == "exclude" && !match
}
func riskText(body []byte) (string, string) {
	if len(body) == 0 {
		return "", "payload_unavailable"
	}
	if len(body) > 1<<20 {
		return "", "payload_too_large"
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "invalid_payload"
	}
	var text strings.Builder
	overflow := false
	unsupportedTool := false
	var visit func(any, int)
	var visitToolOutput func(any, int)
	visitToolOutput = func(value any, depth int) {
		if depth > 64 || overflow {
			overflow = true
			return
		}
		switch v := value.(type) {
		case string:
			visit(v, depth)
		case []any:
			for _, item := range v {
				visitToolOutput(item, depth+1)
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				visitToolOutput(v[key], depth+1)
			}
		}
	}
	visit = func(value any, depth int) {
		if depth > 64 || overflow {
			overflow = true
			return
		}
		switch v := value.(type) {
		case string:
			if text.Len()+len(v) > 64<<10 {
				overflow = true
				return
			}
			text.WriteString(v)
			text.WriteByte(' ')
		case []any:
			for _, item := range v {
				visit(item, depth+1)
			}
		case map[string]any:
			for _, key := range []string{"messages", "contents", "parts", "content", "text", "input", "prompt", "instructions", "system"} {
				if child, ok := v[key]; ok {
					visit(child, depth+1)
				}
			}
			if v["type"] == "function_call_output" || v["type"] == "custom_tool_call_output" {
				if output, ok := v["output"]; ok {
					visitToolOutput(output, depth+1)
				} else {
					unsupportedTool = true
				}
			}
			if raw, exists := v["functionResponse"]; exists {
				if function, ok := raw.(map[string]any); ok {
					if response, ok := function["response"]; ok {
						visitToolOutput(response, depth+2)
					} else {
						unsupportedTool = true
					}
				} else {
					unsupportedTool = true
				}
			}
		}
	}
	visit(payload, 0)
	if overflow {
		return "", "payload_too_large"
	}
	if unsupportedTool {
		return "", "unsupported_tool_payload"
	}
	normalized := strings.Join(strings.Fields(strings.ToLower(text.String())), " ")
	if normalized == "" {
		return "", "payload_unavailable"
	}
	return normalized, ""
}
func (r *riskControl) inspect(req RequestInterceptRequest) RequestInterceptResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.state.Config
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(req.RequestedModel)
	}
	if !c.Enabled || !riskModelMatches(c, model) {
		return RequestInterceptResponse{}
	}
	text, reason := riskText(req.Body)
	refs := []string{}
	digest := ""
	if reason == "" {
		digest = riskDigest(text)
		if c.RememberHashes {
			for _, h := range r.state.Hashes {
				if h == digest {
					reason = "hash"
					break
				}
			}
		}
		if reason == "" {
			for _, word := range c.BlockedKeywords {
				normalized := strings.Join(strings.Fields(strings.ToLower(word)), " ")
				if normalized != "" && strings.Contains(text, normalized) {
					// Keep even a full event history within the sidecar read limit.
					if len(refs) < maxRiskEventRuleRefs {
						refs = append(refs, "kw:"+riskDigest(normalized))
					}
					reason = "keyword"
				}
			}
		}
	}
	if reason == "" {
		return RequestInterceptResponse{}
	}
	decision := "observed"
	uninspected := reason != "keyword" && reason != "hash"
	if uninspected {
		decision = "not_inspected"
	}
	if c.Mode == "pre_block" {
		decision = "blocked"
	}
	model = cleanText(model)
	if len(model) > 200 {
		model = model[:200]
	}
	r.state.Events = append(r.state.Events, riskEvent{At: r.now().UTC(), Decision: decision, Reason: reason, Model: model, CallerRef: "sha256:" + riskDigest(metadataString(req.Metadata, MetadataCallerScope)), RuleRefs: refs})
	if c.RememberHashes && reason == "keyword" && len(r.state.Hashes) < 4096 {
		found := false
		for _, h := range r.state.Hashes {
			found = found || h == digest
		}
		if !found {
			r.state.Hashes = append(r.state.Hashes, digest)
		}
	}
	r.pruneLocked()
	if err := writePrivateJSON(r.path, r.state); err != nil {
		r.storageError = err.Error()
	} else {
		r.storageError = ""
	}
	if c.Mode != "pre_block" {
		return RequestInterceptResponse{}
	}
	return RequestInterceptResponse{Terminate: true, StatusCode: c.BlockStatus, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: refusalBody(req.SourceFormat, refusal{anthropicType: "permission_error", openaiType: "permission_error", openaiCode: "content_policy_violation"}, c.BlockMessage)}
}
func (a *App) getRiskCenter(_ ManagementRequest) ManagementResponse {
	r := a.risk
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	status := map[string]int{"observed": 0, "blocked": 0, "not_inspected": 0, "keyword_hits": 0, "hash_hits": 0, "remembered_hashes": len(r.state.Hashes)}
	for _, e := range r.state.Events {
		status[e.Decision]++
		if e.Reason == "keyword" {
			status["keyword_hits"]++
		}
		if e.Reason == "hash" {
			status["hash_hits"]++
		}
		if e.Decision == "blocked" && e.Reason != "keyword" && e.Reason != "hash" {
			status["not_inspected"]++
		}
	}
	events := append([]riskEvent{}, r.state.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	return JSONResponse(200, map[string]any{"config": r.state.Config, "status": status, "events": events, "storage_error": r.storageError})
}
func (a *App) setRiskConfig(req ManagementRequest) ManagementResponse {
	r := a.risk
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.state
	raw, _ := json.Marshal(r.state.Config)
	next.Config = riskConfig{}
	json.Unmarshal(raw, &next.Config)
	if len(req.Body) > 256<<10 || decodeStrict(req.Body, &next.Config) != nil {
		return JSONError(400, "invalid_risk_policy", "Invalid risk policy")
	}
	if err := validateRiskConfig(&next.Config); err != nil {
		return JSONError(400, "invalid_risk_policy", err.Error())
	}
	if err := writePrivateJSON(r.path, next); err != nil {
		return JSONError(500, "risk_save_failed", err.Error())
	}
	r.state = next
	r.storageError = ""
	return JSONResponse(200, map[string]any{"config": next.Config})
}
func (a *App) clearRiskEvents(_ ManagementRequest) ManagementResponse { return a.clearRisk(false) }
func (a *App) clearRiskHashes(_ ManagementRequest) ManagementResponse { return a.clearRisk(true) }
func (a *App) clearRisk(hashes bool) ManagementResponse {
	r := a.risk
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.state
	if hashes {
		next.Hashes = []string{}
	} else {
		next.Events = []riskEvent{}
	}
	if err := writePrivateJSON(r.path, next); err != nil {
		return JSONError(500, "risk_save_failed", err.Error())
	}
	r.state = next
	r.storageError = ""
	return JSONResponse(200, map[string]bool{"cleared": true})
}
