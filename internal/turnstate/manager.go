// Package turnstate reuses account/model-specific Codex turn-state headers.
// Inspired by github.com/arden-aaai/cpa-plugin-codex-turn-state (MIT).
// See THIRD_PARTY_NOTICES.md for its copyright and license.
package turnstate

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/messages"
)

const Header = "X-Codex-Turn-State"

type Config struct {
	Enabled              bool     `json:"enabled"`
	InjectMode           string   `json:"inject_mode"`
	DryRun               bool     `json:"dry_run"`
	LearnResponses       bool     `json:"learn_responses"`
	TemplateLength       int      `json:"template_length"`
	ReplaceLength        int      `json:"replace_length"`
	TTLSeconds           int      `json:"ttl_seconds"`
	RenewBeforeMinutes   int      `json:"renew_before_minutes"`
	Models               []string `json:"models"`
	ProbeAccounts        []string `json:"probe_accounts"`
	ProbeProxies         []string `json:"probe_proxies"`
	ProbeProxiesRotating []string `json:"probe_proxies_rotating"`
}

func DefaultConfig() Config {
	return Config{InjectMode: "replace-only", LearnResponses: true, TemplateLength: 292,
		ReplaceLength: 312, TTLSeconds: 3600, Models: []string{"gpt6", "gpt-5.6-sol"}, ProbeAccounts: []string{},
		ProbeProxies: []string{}, ProbeProxiesRotating: []string{}}
}

type Template struct {
	Account     string    `json:"account"`
	Model       string    `json:"model"`
	Value       string    `json:"value"`
	IssuedAt    time.Time `json:"issued_at"`
	Source      string    `json:"source,omitempty"`
	Exit        string    `json:"exit,omitempty"`
	HarvestedAt time.Time `json:"harvested_at,omitzero"`
}

type TemplateView struct {
	Account          string    `json:"account"`
	Model            string    `json:"model"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Length           int       `json:"length"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	Source           string    `json:"source,omitempty"`
	Exit             string    `json:"exit,omitempty"`
	HarvestedAt      time.Time `json:"harvested_at,omitzero"`
}

type Decision struct {
	Action        string           `json:"action"`
	Reason        string           `json:"reason"`
	ReasonMessage messages.Message `json:"reason_message,omitzero"`
	Account       string           `json:"account,omitempty"`
	Model         string           `json:"model,omitempty"`
	At            time.Time        `json:"at"`
}

type Counters struct {
	Injected uint64 `json:"injected"`
	Learned  uint64 `json:"learned"`
	Passed   uint64 `json:"passed"`
}

type Status struct {
	Config             Config         `json:"config"`
	Templates          []TemplateView `json:"templates"`
	Counters           Counters       `json:"counters"`
	LastDecision       Decision       `json:"last_decision"`
	ProxyCounts        map[string]int `json:"proxy_counts"`
	ServerTime         time.Time      `json:"server_time"`
	RenewalLeadSeconds int            `json:"renewal_lead_seconds"`
}

type pending struct {
	Account string
	Model   string
	At      time.Time
}

type cooldown struct {
	Until         time.Time `json:"until"`
	Attempts      int       `json:"attempts,omitempty"`
	RenewalBucket string    `json:"renewal_bucket,omitempty"`
}

type diskState struct {
	Version   int                 `json:"version"`
	Config    Config              `json:"config"`
	Templates map[string]Template `json:"templates"`
	Cooldowns map[string]cooldown `json:"cooldowns,omitempty"`
}

// Manager never starts goroutines or timers. Expiry and cooldown pruning run
// synchronously inside host callbacks. Raw state is never returned by Status.
type Manager struct {
	mu              sync.Mutex
	probeMu         sync.Mutex
	path            string
	state           diskState
	pending         map[string]pending
	counters        Counters
	last            Decision
	uploads         map[string]*configUpload
	configRevision  uint64
	revisionToken   string
	revisionTokenAt uint64
	now             func() time.Time
	runProbe        func(Credential, string, string) (ProbeResponse, error)
	activeProbe     *ProbeResult
}

func New() *Manager {
	return &Manager{state: diskState{Version: 1, Config: DefaultConfig(), Templates: map[string]Template{}, Cooldowns: map[string]cooldown{}},
		pending: map[string]pending{}, now: time.Now, runProbe: runHTTPProbe}
}

// Configure stores turn-state data beside (not inside) the billing database.
// Reconfiguration at the same path preserves UI settings without extra I/O.
func (m *Manager) Configure(billingPath string) error {
	return m.ConfigureWith(billingPath, nil)
}

// ConfigureWith validates a new sidecar before apply changes the billing store.
// The state switch commits only if both preparations succeed. Serializing with
// probes prevents an old in-flight request from writing into the new sidecar.
func (m *Manager) ConfigureWith(billingPath string, apply func() error) error {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	path := billingPath + ".turn-state.json"
	if path == m.path {
		if apply != nil {
			return apply()
		}
		return nil
	}
	state := diskState{Version: 1, Config: DefaultConfig(), Templates: map[string]Template{}, Cooldowns: map[string]cooldown{}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return messages.Errorf("Invalid turn-state state file")
		}
		if state.Version != 1 {
			return messages.Errorf("Unsupported turn-state state file version")
		}
		if err := validateConfig(&state.Config); err != nil {
			return messages.Errorf("Invalid turn-state state configuration: %w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return messages.Errorf("Cannot restrict turn-state state file permissions")
		}
	} else if !os.IsNotExist(err) {
		return messages.Errorf("Cannot read the turn-state state file")
	}
	if state.Templates == nil {
		state.Templates = map[string]Template{}
	}
	if state.Cooldowns == nil {
		state.Cooldowns = map[string]cooldown{}
	}
	if apply != nil {
		if err := apply(); err != nil {
			return err
		}
	}
	m.path, m.state = path, state
	m.uploads = nil
	m.configRevision++
	m.pending = map[string]pending{}
	m.counters, m.last = Counters{}, Decision{}
	m.pruneLocked(m.now())
	return nil
}

func validateConfig(cfg *Config) error {
	if cfg.InjectMode != "always" && cfg.InjectMode != "replace-only" {
		return messages.Errorf("inject_mode must be replace-only or always")
	}
	if cfg.TemplateLength < 100 || cfg.TemplateLength > 8192 || cfg.ReplaceLength < 100 || cfg.ReplaceLength > 8192 || cfg.TemplateLength == cfg.ReplaceLength {
		return messages.Errorf("Template and replacement lengths must be different and between 100 and 8192")
	}
	if cfg.TTLSeconds < 60 || cfg.TTLSeconds > 3600 {
		return messages.Errorf("Template lifetime must be 60–3600 seconds and must not exceed the upstream token lifetime")
	}
	if cfg.RenewBeforeMinutes < 0 || cfg.RenewBeforeMinutes > 59 || cfg.RenewBeforeMinutes*60 >= cfg.TTLSeconds {
		return messages.Errorf("Renewal lead must be 0 (automatic) or 1–59 minutes and shorter than the template lifetime")
	}
	var err error
	if cfg.Models, err = cleanList(cfg.Models, 100); err != nil {
		return err
	}
	for i, model := range cfg.Models {
		cfg.Models[i] = ModelName(model)
	}
	cfg.Models, _ = cleanList(cfg.Models, 100)
	if cfg.ProbeAccounts, err = cleanList(cfg.ProbeAccounts, 1000); err != nil {
		return err
	}
	for _, pool := range []*[]string{&cfg.ProbeProxies, &cfg.ProbeProxiesRotating} {
		if *pool, err = cleanList(*pool, MaxProxyPoolEntries); err != nil {
			return err
		}
		for _, proxy := range *pool {
			if err := validateProxy(proxy); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil || len(raw) > MaxConfigBytes {
		return messages.Errorf("Turn-state settings must not exceed 16 MiB")
	}
	return nil
}

func cleanList(values []string, limit int) ([]string, error) {
	if len(values) > limit {
		return nil, messages.Errorf("A configuration list may contain at most %d items", limit)
	}
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, messages.Errorf("A configuration item is too long or contains control characters")
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, nil
}

func validateProxy(value string) error {
	u, err := url.Parse(value)
	if err != nil || u == nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return messages.Errorf("A proxy must be a complete URL: scheme://username:password@host:port")
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return messages.Errorf("Proxy schemes are limited to http, https, socks5, and socks5h")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return messages.Errorf("The proxy port must be between 1 and 65535")
		}
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return messages.Errorf("The proxy contains control characters")
	}
	return nil
}

// Update accepts a partial JSON configuration. Omitted proxy pools keep their
// credentials; [] explicitly clears a pool. Masked status URLs cannot be saved.
func (m *Manager) Update(raw []byte) error {
	// Do not let a completed probe commit a result under a newer configuration.
	// Probe holds this same gate for the whole bounded HTTP call.
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateLocked(raw)
}

func (m *Manager) updateLocked(raw []byte) error {
	if len(raw) > MaxConfigBytes {
		return messages.Errorf("Turn-state settings must not exceed 16 MiB")
	}
	cfg := cloneConfig(m.state.Config)
	input := struct {
		*Config
		ExpectedRevision string `json:"expected_revision,omitempty"`
	}{Config: &cfg}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return messages.Errorf("Invalid turn-state configuration format")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return messages.Errorf("Turn-state configuration must contain exactly one JSON object")
	}
	if input.ExpectedRevision != "" && input.ExpectedRevision != m.configRevisionTokenLocked() {
		return messages.Errorf("Settings changed since proxies were loaded; reload the saved proxies and retry")
	}
	if err := validateConfig(&cfg); err != nil {
		return err
	}
	for _, pool := range [][]string{cfg.ProbeProxies, cfg.ProbeProxiesRotating} {
		for _, proxy := range pool {
			if strings.Contains(proxy, "***") {
				return messages.Errorf("Enter the complete proxy URL; omit the proxy field to preserve its current value")
			}
		}
	}
	old := m.state.Config
	oldTemplates := m.state.Templates
	oldCooldowns := m.state.Cooldowns
	m.state.Cooldowns = make(map[string]cooldown, len(oldCooldowns))
	for k, v := range oldCooldowns {
		m.state.Cooldowns[k] = v
	}
	m.state.Templates = make(map[string]Template, len(oldTemplates))
	for k, v := range oldTemplates {
		m.state.Templates[k] = v
	}
	m.state.Config = cfg
	if old.TTLSeconds != cfg.TTLSeconds || old.RenewBeforeMinutes != cfg.RenewBeforeMinutes {
		m.rescheduleRenewalsLocked()
	}
	m.pruneLocked(m.now())
	if err := m.persistLocked(); err != nil {
		m.state.Config = old
		m.state.Templates = oldTemplates
		m.state.Cooldowns = oldCooldowns
		return err
	}
	m.configRevision++
	return nil
}

func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Config.Enabled
}

func cloneConfig(cfg Config) Config {
	cfg.Models = append([]string{}, cfg.Models...)
	cfg.ProbeAccounts = append([]string{}, cfg.ProbeAccounts...)
	cfg.ProbeProxies = append([]string{}, cfg.ProbeProxies...)
	cfg.ProbeProxiesRotating = append([]string{}, cfg.ProbeProxiesRotating...)
	return cfg
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneLocked(now)
	cfg := m.state.Config
	cfg.Models = append([]string{}, cfg.Models...)
	cfg.ProbeAccounts = append([]string{}, cfg.ProbeAccounts...)
	counts := map[string]int{"static": len(cfg.ProbeProxies), "rotating": len(cfg.ProbeProxiesRotating)}
	// Counts are sufficient for the editor, which never reloads saved secrets.
	// Returning large masked pools makes every status refresh unnecessarily big.
	cfg.ProbeProxies, cfg.ProbeProxiesRotating = []string{}, []string{}
	rows := []TemplateView{}
	for _, t := range m.state.Templates {
		expiresAt := t.IssuedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second)
		rows = append(rows, TemplateView{Account: t.Account, Model: t.Model, IssuedAt: t.IssuedAt,
			ExpiresAt: expiresAt, Length: len(t.Value), RemainingSeconds: int64(expiresAt.Sub(now) / time.Second),
			Source: t.Source, Exit: maskSavedExit(t.Exit), HarvestedAt: t.HarvestedAt})
	}
	sort.Slice(rows, func(i, j int) bool { return key(rows[i].Account, rows[i].Model) < key(rows[j].Account, rows[j].Model) })
	return Status{Config: cfg, Templates: rows, Counters: m.counters, LastDecision: m.last, ProxyCounts: counts,
		ServerTime: now, RenewalLeadSeconds: int(configRenewalLead(cfg) / time.Second)}
}

func maskSavedExit(exit string) string {
	if exit == "" || exit == "direct" {
		return exit
	}
	return maskProxy(exit)
}

func maskProxy(raw string) string {
	if raw == "" {
		return "direct"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid proxy"
	}
	if u.User != nil {
		u.User = nil
		return u.Scheme + "://***@" + u.Host
	}
	return u.String()
}

func key(account, model string) string { return account + "\x00" + model }

func validBucket(account, model string) bool {
	return account != "" && model != "" && len(account) <= 4096 && len(model) <= 4096 && !strings.ContainsAny(account+model, "\x00\r\n")
}

// ModelName removes only CPA's documented thinking controls. Other parentheses
// and real model suffixes stay distinct; no client aliases are resolved here.
func ModelName(model string) string {
	model = strings.TrimSpace(model)
	start := strings.LastIndexByte(model, '(')
	if start <= 0 || !strings.HasSuffix(model, ")") {
		return model
	}
	suffix := strings.ToLower(model[start+1 : len(model)-1])
	switch suffix {
	case "none", "auto", "-1", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return model[:start]
	}
	if value, err := strconv.Atoi(suffix); err == nil && value >= 0 {
		return model[:start]
	}
	return model
}

func issuedAt(value string) (time.Time, bool) {
	raw, err := base64.URLEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(value)
	}
	// Fernet layout: version, timestamp, IV, at least one AES block, HMAC.
	if err != nil || len(raw) < 73 || raw[0] != 0x80 || (len(raw)-57)%16 != 0 {
		return time.Time{}, false
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds > 1<<63-1 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0).UTC(), true
}

func (m *Manager) usableLocked(t Template, now time.Time) bool {
	issued, ok := issuedAt(t.Value)
	return ok && validBucket(t.Account, t.Model) && len(t.Value) == m.state.Config.TemplateLength && issued.Equal(t.IssuedAt) &&
		!issued.After(now) && now.Sub(issued) < time.Duration(m.state.Config.TTLSeconds)*time.Second
}

func (m *Manager) pruneLocked(now time.Time) {
	m.pruneConfigUploadsLocked()
	for k, t := range m.state.Templates {
		if k != key(t.Account, t.Model) || !m.usableLocked(t, now) {
			delete(m.state.Templates, k)
		}
	}
	for id, p := range m.pending {
		if now.Sub(p.At) > 15*time.Minute {
			delete(m.pending, id)
		}
	}
	for k, c := range m.state.Cooldowns {
		if !c.Until.After(now) {
			delete(m.state.Cooldowns, k)
		}
	}
}

// Before sees only the exact account and upstream model selected by CPA. It
// never infers attribution from enabled account counts or timing.
func (m *Manager) Before(requestID, account, model string, headers http.Header) (http.Header, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Config.Enabled {
		return nil, nil
	}
	now := m.now()
	model = ModelName(model)
	m.pruneLocked(now)
	if !validBucket(account, model) {
		m.recordLocked("pass", "Cannot verify the selected account and upstream model", account, model, now)
		return nil, nil
	}
	if requestID != "" && len(m.pending) < 4096 {
		m.pending[requestID] = pending{Account: account, Model: model, At: now}
	}
	value := headerValue(headers)
	t, ok := m.state.Templates[key(account, model)]
	if !ok {
		m.recordLocked("pass", "There is no valid template for this account and model", account, model, now)
		return nil, nil
	}
	if value == t.Value {
		m.recordLocked("pass", "The request already carries the current template", account, model, now)
		return nil, nil
	}
	if m.state.Config.InjectMode == "replace-only" && len(value) != m.state.Config.ReplaceLength {
		m.recordLocked("pass", "replace-only replaces only request headers of the configured length", account, model, now)
		return nil, nil
	}
	if m.state.Config.DryRun {
		m.recordLocked("dry_run", "A template is available; observe mode left the request unchanged", account, model, now)
		return nil, nil
	}
	m.recordLocked("inject", "A valid template for the same account and model was injected", account, model, now)
	return http.Header{Header: []string{t.Value}}, []string{Header}
}

// Learn receives raw response headers, not response bodies or usage. A request
// ID transfers the exact selected account AND model across host hook boundaries.
func (m *Manager) Learn(requestID, account, model string, headers http.Header) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.Config.Enabled || !m.state.Config.LearnResponses {
		return nil
	}
	now := m.now()
	m.pruneLocked(now)
	p, remembered := m.pending[requestID]
	delete(m.pending, requestID)
	if !remembered {
		if headerValue(headers) != "" {
			m.recordLocked("skip", "The Codex request has no verifiable account attribution", account, model, now)
		}
		return nil
	} else {
		if account != "" && account != p.Account {
			m.recordLocked("skip", "The response account does not match the request attribution", account, model, now)
			return nil
		}
		account, model = p.Account, p.Model
	}
	value := headerValue(headers)
	if value == "" {
		return nil
	}
	return m.learnLocked(account, model, value, now)
}

func (m *Manager) learnLocked(account, model, value string, now time.Time) error {
	return m.learnFromLocked(account, model, value, now, "response", "")
}

func (m *Manager) learnFromLocked(account, model, value string, now time.Time, source, exit string) error {
	if !validBucket(account, model) {
		m.recordLocked("skip", "Cannot verify the response account and upstream model", account, model, now)
		return nil
	}
	if len(value) != m.state.Config.TemplateLength {
		m.recordLocked("skip", "The response header length does not match the template length", account, model, now)
		return nil
	}
	timestamp, ok := issuedAt(value)
	if !ok || timestamp.After(now) || now.Sub(timestamp) >= time.Duration(m.state.Config.TTLSeconds)*time.Second {
		m.recordLocked("skip", "The template timestamp is invalid, in the future, or expired", account, model, now)
		return nil
	}
	k := key(account, model)
	old, exists := m.state.Templates[k]
	if exists && !timestamp.After(old.IssuedAt) {
		return nil
	}
	m.state.Templates[k] = Template{Account: account, Model: model, Value: value, IssuedAt: timestamp,
		Source: source, Exit: exit, HarvestedAt: now}
	if err := m.persistLocked(); err != nil {
		if exists {
			m.state.Templates[k] = old
		} else {
			delete(m.state.Templates, k)
		}
		m.recordLocked("error", "Cannot save the template", account, model, now)
		return err
	}
	m.recordLocked("harvest", "A valid template was saved", account, model, now)
	return nil
}

func (m *Manager) Complete(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, requestID)
}

func (m *Manager) Clear(account, model string) error {
	// Clear must not race with a probe whose response is about to be committed.
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if (account == "") != (model == "") {
		return messages.Errorf("Both account and model are required to clear a single template")
	}
	old := m.state.Templates
	m.state.Templates = map[string]Template{}
	for k, t := range old {
		if account != "" && k != key(account, model) {
			m.state.Templates[k] = t
		}
	}
	if err := m.persistLocked(); err != nil {
		m.state.Templates = old
		return err
	}
	return nil
}

func (m *Manager) recordLocked(action, reason, account, model string, now time.Time) {
	m.last = Decision{Action: action, Reason: reason, ReasonMessage: messages.Literal(reason), Account: account, Model: model, At: now}
	switch action {
	case "inject":
		m.counters.Injected++
	case "harvest":
		m.counters.Learned++
	case "pass", "dry_run":
		m.counters.Passed++
	}
}

func headerValue(headers http.Header) string {
	for k, values := range headers {
		if strings.EqualFold(k, Header) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func (m *Manager) persistLocked() error {
	if m.path == "" {
		return messages.Errorf("The turn-state state file has not been configured")
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return messages.Errorf("Cannot create the turn-state state directory")
	}
	raw, err := json.Marshal(m.state)
	if err != nil {
		return messages.Errorf("Cannot encode the turn-state state")
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".turn-state-*")
	if err != nil {
		return messages.Errorf("Cannot create a temporary turn-state file")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return messages.Errorf("Cannot write the turn-state state")
	}
	if err := os.Rename(f.Name(), m.path); err != nil {
		return messages.Errorf("Cannot replace the turn-state state file")
	}
	return nil
}
