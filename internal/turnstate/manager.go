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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cpa-key-billing/internal/messages"
)

const Header = "X-Codex-Turn-State"

type Config struct {
	// Suspended is the global State switch. It leaves every other setting,
	// template and cooldown untouched so resuming restores the prior behavior.
	Suspended                    bool     `json:"suspended"`
	ForceAstra                   bool     `json:"force_astra"`
	Enabled                      bool     `json:"enabled"`
	InjectMode                   string   `json:"inject_mode"`
	DryRun                       bool     `json:"dry_run"`
	LearnResponses               bool     `json:"learn_responses"`
	InjectCookies                bool     `json:"inject_cookies"`
	BreakoutRetry                bool     `json:"breakout_retry"`
	ProbeParallel                int      `json:"probe_parallel"`
	Berserk                      bool     `json:"berserk"`
	BerserkMinutes               int      `json:"berserk_minutes"`
	TemplateLength               int      `json:"template_length"`
	ReplaceLength                int      `json:"replace_length"`
	TTLSeconds                   int      `json:"ttl_seconds"`
	RenewBeforeMinutes           int      `json:"renew_before_minutes"`
	Models                       []string `json:"models"`
	ProbeAccounts                []string `json:"probe_accounts"`
	ProbeProxies                 []string `json:"probe_proxies"`
	ProbeProxiesRotating         []string `json:"probe_proxies_rotating"`
	ProbeDropFailedProxies       bool     `json:"probe_drop_failed_proxies"`
	ProbeDropDegradedProxies     bool     `json:"probe_drop_degraded_proxies"`
	ProbeMinProxies              int      `json:"probe_min_proxies"`
	ProbeHourlyLimit             int      `json:"probe_hourly_limit"`
	ProbeVerifyCompletion        bool     `json:"probe_verify_completion"`
	ProbeStaticCooldownMinutes   int      `json:"probe_static_cooldown_minutes"`
	ProbeRotatingCooldownMinutes int      `json:"probe_rotating_cooldown_minutes"`
	ProbeAccountCooldownMinutes  int      `json:"probe_account_cooldown_minutes"`
	ProbeRotatingMaxAttempts     int      `json:"probe_rotating_max_attempts"`
}

func DefaultConfig() Config {
	return Config{InjectMode: "replace-only", LearnResponses: true, InjectCookies: true, ProbeParallel: 3, BerserkMinutes: 1, TemplateLength: 292,
		ReplaceLength: 312, TTLSeconds: 3600, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}, ProbeAccounts: []string{},
		ProbeProxies: []string{}, ProbeProxiesRotating: []string{}, ProbeMinProxies: 10,
		ProbeStaticCooldownMinutes: 55, ProbeRotatingCooldownMinutes: 10,
		ProbeAccountCooldownMinutes: 10, ProbeRotatingMaxAttempts: 10}
}

type Template struct {
	Account     string    `json:"account"`
	Model       string    `json:"model"`
	Value       string    `json:"value"`
	IssuedAt    time.Time `json:"issued_at"`
	Source      string    `json:"source,omitempty"`
	Exit        string    `json:"exit,omitempty"`
	HarvestedAt time.Time `json:"harvested_at,omitzero"`
	// Cookies holds the name=value pairs the harvesting response set. They are
	// sent with the template so later turns look like the same upstream session.
	Cookies string `json:"cookies,omitempty"`
	// ExitKey identifies the exit that harvested this template, so renewal
	// tries the same exit first. It is a hash and never holds the proxy URL.
	ExitKey string `json:"exit_key,omitempty"`
}

type TemplateView struct {
	Fingerprint      string    `json:"fingerprint"`
	Account          string    `json:"account"`
	Model            string    `json:"model"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Length           int       `json:"length"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	Source           string    `json:"source,omitempty"`
	Exit             string    `json:"exit,omitempty"`
	HarvestedAt      time.Time `json:"harvested_at,omitzero"`
	Cookies          int       `json:"cookies,omitempty"`
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
	Injected    uint64    `json:"injected"`
	Learned     uint64    `json:"learned"`
	Passed      uint64    `json:"passed"`
	Substituted uint64    `json:"substituted"`
	Inserted    uint64    `json:"inserted"`
	Skipped     uint64    `json:"skipped"`
	DryRun      uint64    `json:"dry_run"`
	Errors      uint64    `json:"errors"`
	Since       time.Time `json:"since"`
}

type Status struct {
	Config              Config            `json:"config"`
	Templates           []TemplateView    `json:"templates"`
	Counters            Counters          `json:"counters"`
	LastDecision        Decision          `json:"last_decision"`
	ProxyCounts         map[string]int    `json:"proxy_counts"`
	ProxyConfigRevision string            `json:"proxy_config_revision"`
	ServerTime          time.Time         `json:"server_time"`
	RenewalLeadSeconds  int               `json:"renewal_lead_seconds"`
	ProbeStats          ProbeStats        `json:"probe_stats"`
	LastProbe           ProbeResult       `json:"last_probe"`
	ProbeProgress       ProbeProgress     `json:"probe_progress"`
	ProbeBudget         ProbeBudget       `json:"probe_budget"`
	PendingLearnedCount int               `json:"pending_learned_count"`
	PersistenceError    string            `json:"persistence_error,omitempty"`
	Observations        ObservationStatus `json:"observations"`
}

type pending struct {
	Account string
	Model   string
	At      time.Time
	Epoch   uint64
	Wrote   bool
}

type cooldown struct {
	Until time.Time `json:"until"`
	// PacingUntil keeps the short per-account request interval independent
	// of optional failure pauses. Legacy cooldowns have no known interval.
	PacingUntil   time.Time `json:"pacing_until,omitzero"`
	Attempts      int       `json:"attempts,omitempty"`
	RenewalBucket string    `json:"renewal_bucket,omitempty"`
}

type diskState struct {
	Version      int                          `json:"version"`
	Defaults     int                          `json:"defaults,omitempty"`
	CheckpointID string                       `json:"checkpoint_id,omitempty"`
	Config       Config                       `json:"config"`
	Templates    map[string]Template          `json:"templates"`
	Cooldowns    map[string]cooldown          `json:"cooldowns,omitempty"`
	ProxyCursor  probeProxyCursor             `json:"proxy_cursor,omitzero"`
	ProbeUsage   []probeUsage                 `json:"probe_usage,omitempty"`
	Discarded    map[string]discardedTemplate `json:"discarded_templates,omitempty"`
}

// Manager never starts goroutines or timers. Expiry and cooldown pruning run
// synchronously inside host callbacks. Raw state is never returned by Status.
type Manager struct {
	mu                sync.Mutex
	probeMu           sync.RWMutex // Probes share it; configuration writers take it exclusively.
	probing           int
	probingBuckets    map[string]int // Buckets with a probe in flight.
	writerMu          sync.Mutex
	path              string
	state             diskState
	pending           map[string]pending
	counters          Counters
	last              Decision
	uploads           map[string]*configUpload
	configRevision    uint64
	revisionToken     string
	revisionTokenAt   uint64
	now               func() time.Time
	runProbe          func(Credential, string, string) (ProbeResponse, error)
	activeProbe       *ProbeResult
	probeStats        ProbeStats
	lastProbe         ProbeResult
	basePath          string
	baseDigest        string
	dirtyTemplates    map[string]Template
	templateEpoch     uint64
	allTemplateEpoch  uint64
	clearedTemplates  map[string]uint64
	lastProbeBucket   string
	lastMissingBucket string
	lastRenewBucket   string
	renewalBurst      int
	prunedAt          time.Time
	writeState        func(string, []byte) error
	persistenceError  string
	runtimeDirty      bool
	observations      observationState
	suspended         atomic.Bool // Mirrors state.Config.Suspended for lock-free hot paths.
}

func New() *Manager {
	return &Manager{state: diskState{Version: 1, Defaults: defaultsRevision, Config: DefaultConfig(), Templates: map[string]Template{}, Cooldowns: map[string]cooldown{}},
		pending: map[string]pending{}, dirtyTemplates: map[string]Template{}, clearedTemplates: map[string]uint64{}, now: time.Now, runProbe: runHTTPProbe}
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
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	path := billingPath + ".turn-state.json"
	if path == m.path {
		if apply != nil {
			m.mu.Unlock()
			err := apply()
			m.mu.Lock()
			return err
		}
		return nil
	}
	// Loading/validating a different sidecar and the companion configuration
	// must not hold the mutex used by active business requests.
	m.mu.Unlock()
	// Files written before inject_cookies or the short lifetime existed are
	// decoded over the settings they were saved with, not the current defaults.
	state := diskState{Version: 1, Config: legacyConfig(), Templates: map[string]Template{}, Cooldowns: map[string]cooldown{}}
	base, migrated := "", false
	load := func() error {
		raw, err := os.ReadFile(path)
		if err == nil {
			base = stateDigest(raw)
			if err := json.Unmarshal(raw, &state); err != nil {
				return messages.Errorf("Invalid turn-state state file")
			}
			if state.Version != 1 {
				return messages.Errorf("Unsupported turn-state state file version")
			}
			// Correct only the shipped legacy default. Operator aliases, custom
			// selections and intentionally empty scopes remain unchanged.
			if len(state.Config.Models) == 2 && state.Config.Models[0] == "gpt6" && state.Config.Models[1] == "gpt-5.6-sol" {
				state.Config.Models = DefaultConfig().Models
			}
			migrated = migrateDefaults(&state)
			if err := validateConfig(&state.Config); err != nil {
				return messages.Errorf("Invalid turn-state state configuration: %w", err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return messages.Errorf("Cannot restrict turn-state state file permissions")
			}
		} else if !os.IsNotExist(err) {
			return messages.Errorf("Cannot read the turn-state state file")
		} else if _, runtimeErr := os.Stat(path + ".runtime.json"); runtimeErr == nil || !os.IsNotExist(runtimeErr) {
			// Restoring an overlay without its base must fail explicitly instead
			// of silently treating durable templates as an empty new setup.
			return messages.Errorf("Invalid turn-state state file")
		} else {
			state.Config, state.Defaults = shippedConfig(), defaultsRevision
		}
		if state.Templates == nil {
			state.Templates = map[string]Template{}
		}
		if state.Cooldowns == nil {
			state.Cooldowns = map[string]cooldown{}
		}
		if err := validateDiscarded(state.Discarded); err != nil {
			return err
		}
		if base != "" {
			if err := loadRuntime(path, base, &state, m.now()); err != nil {
				return err
			}
		}
		if migrated {
			rescheduleRenewals(&state)
		}
		if err := validateProbeUsage(state.ProbeUsage); err != nil {
			return err
		}
		if apply != nil {
			if err := apply(); err != nil {
				return err
			}
		}
		return nil
	}
	err := load()
	m.mu.Lock()
	if err != nil {
		return err
	}
	m.path, m.state = path, state
	m.suspended.Store(state.Config.Suspended)
	m.basePath, m.baseDigest = path, base
	m.dirtyTemplates = map[string]Template{}
	m.persistenceError = ""
	m.runtimeDirty = false
	m.templateEpoch++
	m.allTemplateEpoch = m.templateEpoch
	m.clearedTemplates = map[string]uint64{}
	m.lastProbeBucket, m.lastRenewBucket, m.lastMissingBucket = "", "", ""
	m.prunedAt = time.Time{}
	m.uploads = nil
	m.configRevision++
	m.pending = map[string]pending{}
	m.observations = observationState{Since: m.now().UTC()}
	m.counters, m.last = Counters{Since: m.now().UTC()}, Decision{}
	m.probeStats, m.lastProbe = ProbeStats{Since: m.now().UTC()}, ProbeResult{}
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
	if cfg.ProbeParallel < 1 || cfg.ProbeParallel > BerserkConcurrency {
		return messages.Errorf("Concurrent probes must be between 1 and 10")
	}
	if cfg.BerserkMinutes < 1 || cfg.BerserkMinutes > 59 || cfg.Berserk && cfg.BerserkMinutes*60 >= cfg.TTLSeconds {
		return messages.Errorf("Berserk mode must start 1–59 minutes before expiry and within the template lifetime")
	}
	if cfg.ProbeMinProxies < 1 || cfg.ProbeMinProxies > 2*MaxProxyPoolEntries {
		return messages.Errorf("The minimum retained proxy count must be between 1 and 40000")
	}
	if cfg.ProbeHourlyLimit < 0 || cfg.ProbeHourlyLimit > 10000 {
		return messages.Errorf("The hourly probe limit must be 0 (unlimited) or between 1 and 10000")
	}
	for _, minutes := range []int{cfg.ProbeStaticCooldownMinutes, cfg.ProbeRotatingCooldownMinutes, cfg.ProbeAccountCooldownMinutes} {
		if minutes < 0 || minutes > 1440 {
			return messages.Errorf("Probe cooldowns must be 0 (disabled) or between 1 and 1440 minutes")
		}
	}
	if cfg.ProbeRotatingMaxAttempts < 0 || cfg.ProbeRotatingMaxAttempts > 100 {
		return messages.Errorf("The rotating proxy attempt limit must be 0 (unlimited) or between 1 and 100")
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
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateLocked(raw)
}

func (m *Manager) updateLocked(raw []byte) error {
	if len(raw) > MaxConfigBytes {
		return messages.Errorf("Turn-state settings must not exceed 16 MiB")
	}
	currentConfig := m.state.Config
	currentRevision := m.configRevisionTokenLocked()
	m.mu.Unlock()
	cfg := cloneConfig(currentConfig)
	validate := func() error {
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
		if input.ExpectedRevision != "" && input.ExpectedRevision != currentRevision {
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
		return nil
	}
	err := validate()
	m.mu.Lock()
	if err != nil {
		return err
	}
	next := cloneState(m.state)
	// A larger TTL must not resurrect a template that had already expired
	// under the previously active policy, even between lazy cleanup passes.
	pruneState(&next, m.now())
	// Cooldown policy edits apply to future reservations. Preserve existing
	// deadlines and attempt counts; shortening a setting must not silently
	// clear a refusal or reset either rotating or hourly probe budgets.
	// A zero setting bypasses its failure gate without deleting saved records.
	next.Config = cfg
	pruneState(&next, m.now())
	if currentConfig.TTLSeconds != cfg.TTLSeconds || currentConfig.RenewBeforeMinutes != cfg.RenewBeforeMinutes {
		rescheduleRenewals(&next)
	}
	if err := m.commitStateLocked(next, true, ""); err != nil {
		return err
	}
	// Existing in-flight responses cannot repopulate templates invalidated by
	// a template-rule change after the new settings become active.
	if currentConfig.TTLSeconds != cfg.TTLSeconds || currentConfig.TemplateLength != cfg.TemplateLength || currentConfig.LearnResponses != cfg.LearnResponses || currentConfig.Enabled != cfg.Enabled {
		m.templateEpoch++
		m.allTemplateEpoch = m.templateEpoch
		m.clearedTemplates = map[string]uint64{}
	}
	m.configRevision++
	return nil
}

// Active reports the global State switch without taking the request mutex, so
// host hooks can return before decoding a payload while State is suspended.
func (m *Manager) Active() bool {
	return m != nil && !m.suspended.Load()
}

func (m *Manager) Enabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Config.Enabled
}

// ForceAstraEnabled is independent of template injection and observation mode.
func (m *Manager) ForceAstraEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Config.ForceAstra
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
	// The editor loads credentials separately through revision-checked pages.
	// A small revision lets it detect other tabs or a lost deletion response
	// without returning the full pools on every status refresh.
	cfg.ProbeProxies, cfg.ProbeProxiesRotating = []string{}, []string{}
	rows := []TemplateView{}
	for _, t := range m.state.Templates {
		if !m.usableLocked(t, now) {
			continue
		}
		expiresAt := t.IssuedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second)
		rows = append(rows, TemplateView{Account: t.Account, Model: t.Model, IssuedAt: t.IssuedAt, Fingerprint: templateFingerprint(t.Value),
			ExpiresAt: expiresAt, Length: len(t.Value), RemainingSeconds: int64(expiresAt.Sub(now) / time.Second),
			Source: t.Source, Exit: maskSavedExit(t.Exit), HarvestedAt: t.HarvestedAt, Cookies: cookieCount(t.Cookies)})
	}
	sort.Slice(rows, func(i, j int) bool { return key(rows[i].Account, rows[i].Model) < key(rows[j].Account, rows[j].Model) })
	progress := ProbeProgress{}
	if m.activeProbe != nil {
		progress = ProbeProgress{Active: true, Result: *m.activeProbe}
	}
	return Status{Config: cfg, Templates: rows, Counters: m.counters, LastDecision: m.last, ProxyCounts: counts,
		PendingLearnedCount: len(m.dirtyTemplates), PersistenceError: m.persistenceError,
		ProxyConfigRevision: m.configRevisionTokenLocked(),
		ServerTime:          now, RenewalLeadSeconds: int(configRenewalLead(cfg) / time.Second),
		ProbeStats: m.probeStats, LastProbe: m.lastProbe, ProbeProgress: progress,
		ProbeBudget: probeBudget(m.state, now), Observations: m.observationStatusLocked(now)}
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
	return usableWithConfig(t, m.state.Config, now) && !templateDiscarded(m.state, t.Account, t.Model, t.Value, now)
}

func usableWithConfig(t Template, cfg Config, now time.Time) bool {
	issued, ok := issuedAt(t.Value)
	return ok && validBucket(t.Account, t.Model) && len(t.Value) == cfg.TemplateLength && issued.Equal(t.IssuedAt) &&
		!issued.After(now) && now.Sub(issued) < time.Duration(cfg.TTLSeconds)*time.Second
}

func (m *Manager) pruneLocked(now time.Time) {
	// Request admission validates its exact template independently. Full cache
	// cleanup is opportunistic, at most once per minute, not once per token or
	// request and never by a background task.
	if !m.prunedAt.IsZero() && !now.Before(m.prunedAt) && now.Sub(m.prunedAt) < time.Minute {
		return
	}
	m.prunedAt = now
	m.pruneConfigUploadsLocked()
	pruneDiscarded(&m.state, now)
	for k, t := range m.state.Templates {
		if k != key(t.Account, t.Model) || !m.usableLocked(t, now) {
			delete(m.state.Templates, k)
			delete(m.dirtyTemplates, k)
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
	return m.beforeLocked(requestID, account, model, headers)
}

func (m *Manager) beforeLocked(requestID, account, model string, headers http.Header) (http.Header, []string) {
	return m.beforeAtLocked(requestID, account, model, headers, m.now())
}

func (m *Manager) beforeAtLocked(requestID, account, model string, headers http.Header, now time.Time) (http.Header, []string) {
	// Retries reuse the ID. An unidentifiable or disabled pass must not leave
	// the previous account or its injection flag attached to a later response.
	delete(m.pending, requestID)
	if !m.state.Config.Enabled {
		return nil, nil
	}
	model = ModelName(model)
	m.pruneLocked(now)
	if !validBucket(account, model) {
		m.recordLocked("pass", "Cannot verify the selected account and upstream model", account, model, now)
		return nil, nil
	}
	if requestID != "" && len(m.pending) < 4096 {
		m.pending[requestID] = pending{Account: account, Model: model, At: now, Epoch: m.templateEpoch}
	}
	value := headerValue(headers)
	t, ok := m.state.Templates[key(account, model)]
	if !ok || !m.usableLocked(t, now) {
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
	if value == "" {
		m.counters.Inserted++
	} else {
		m.counters.Substituted++
	}
	if p, ok := m.pending[requestID]; ok {
		p.Wrote = true
		m.pending[requestID] = p
	}
	if cookies := injectedCookies(headers, t, m.state.Config); cookies != "" {
		return http.Header{Header: []string{t.Value}, "Cookie": []string{cookies}}, []string{Header, "Cookie"}
	}
	return http.Header{Header: []string{t.Value}}, []string{Header}
}

// Learn receives raw response headers, not response bodies or usage. A request
// ID transfers the exact selected account AND model across host hook boundaries.
func (m *Manager) Learn(requestID, account, model string, headers http.Header) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, remembered := m.pending[requestID]
	delete(m.pending, requestID)
	if !m.state.Config.Enabled {
		return nil
	}
	now := m.now()
	m.pruneLocked(now)
	if !remembered || now.Sub(p.At) > 15*time.Minute || p.Epoch < m.allTemplateEpoch || p.Epoch < m.clearedTemplates[key(p.Account, p.Model)] {
		if headerValue(headers) != "" && (remembered || account == "" || m.inScopeLocked(account, ModelName(model))) {
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
	m.recordObservationLocked(p, len(value), now)
	if value == "" || !m.state.Config.LearnResponses {
		return nil
	}
	return m.learnFromLocked(account, model, value, responseCookies(headers), now, "response", "")
}

func (m *Manager) learnFromLocked(account, model, value, cookies string, now time.Time, source, exit string) error {
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
	if templateDiscarded(m.state, account, model, value, now) {
		m.recordLocked("skip", "The template was discarded by an administrator and will not be learned again", account, model, now)
		return nil
	}
	old, exists := m.state.Templates[k]
	if exists && !timestamp.After(old.IssuedAt) {
		return nil
	}
	m.state.Templates[k] = Template{Account: account, Model: model, Value: value, IssuedAt: timestamp,
		Source: source, Exit: exit, HarvestedAt: now, Cookies: cookies}
	m.dirtyTemplates[k] = m.state.Templates[k]
	m.recordLocked("harvest", "A valid response template was cached; management synchronization will persist it", account, model, now)
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
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if (account == "") != (model == "") {
		return messages.Errorf("Both account and model are required to clear a single template")
	}
	next := cloneState(m.state)
	scope := "*"
	if account == "" {
		next.Templates = map[string]Template{}
	} else {
		scope = key(account, model)
		delete(next.Templates, scope)
	}
	return m.commitStateLocked(next, false, scope)
}

func (m *Manager) recordLocked(action, reason, account, model string, now time.Time) {
	m.last = Decision{Action: action, Reason: reason, ReasonMessage: messages.Literal(reason), Account: account, Model: model, At: now}
	switch action {
	case "inject":
		m.counters.Injected++
	case "harvest":
		m.counters.Learned++
	case "pass":
		m.counters.Passed++
	case "dry_run":
		m.counters.Passed++ // Preserve the existing aggregate for old clients.
		m.counters.DryRun++
	case "skip":
		m.counters.Skipped++
	case "error":
		m.counters.Errors++
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
