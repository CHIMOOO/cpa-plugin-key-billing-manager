// Package turnstate reuses account/model-specific Codex turn-state headers.
// Inspired by github.com/arden-aaai/cpa-plugin-codex-turn-state (MIT).
// See THIRD_PARTY_NOTICES.md for its copyright and license.
package turnstate

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	Models               []string `json:"models"`
	ProbeAccounts        []string `json:"probe_accounts"`
	ProbeProxies         []string `json:"probe_proxies"`
	ProbeProxiesRotating []string `json:"probe_proxies_rotating"`
}

func DefaultConfig() Config {
	return Config{InjectMode: "replace-only", LearnResponses: true, TemplateLength: 292,
		ReplaceLength: 312, TTLSeconds: 3600, Models: []string{}, ProbeAccounts: []string{},
		ProbeProxies: []string{}, ProbeProxiesRotating: []string{}}
}

type Template struct {
	Account  string    `json:"account"`
	Model    string    `json:"model"`
	Value    string    `json:"value"`
	IssuedAt time.Time `json:"issued_at"`
}

type TemplateView struct {
	Account   string    `json:"account"`
	Model     string    `json:"model"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Length    int       `json:"length"`
}

type Decision struct {
	Action  string    `json:"action"`
	Reason  string    `json:"reason"`
	Account string    `json:"account,omitempty"`
	Model   string    `json:"model,omitempty"`
	At      time.Time `json:"at"`
}

type Counters struct {
	Injected uint64 `json:"injected"`
	Learned  uint64 `json:"learned"`
	Passed   uint64 `json:"passed"`
}

type Status struct {
	Config       Config         `json:"config"`
	Templates    []TemplateView `json:"templates"`
	Counters     Counters       `json:"counters"`
	LastDecision Decision       `json:"last_decision"`
	ProxyCounts  map[string]int `json:"proxy_counts"`
}

type pending struct {
	Account string
	Model   string
	At      time.Time
}

type cooldown struct {
	Until    time.Time `json:"until"`
	Attempts int       `json:"attempts,omitempty"`
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
	mu       sync.Mutex
	probeMu  sync.Mutex
	path     string
	state    diskState
	pending  map[string]pending
	counters Counters
	last     Decision
	now      func() time.Time
	runProbe func(Credential, string, string) (ProbeResponse, error)
}

func New() *Manager {
	return &Manager{state: diskState{Version: 1, Config: DefaultConfig(), Templates: map[string]Template{}, Cooldowns: map[string]cooldown{}},
		pending: map[string]pending{}, now: time.Now, runProbe: runCurlProbe}
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
			return errors.New("turn-state 状态文件无效")
		}
		if state.Version != 1 {
			return errors.New("turn-state 状态文件版本不受支持")
		}
		if err := validateConfig(&state.Config); err != nil {
			return fmt.Errorf("turn-state 状态配置无效：%w", err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return errors.New("无法限制 turn-state 状态文件权限")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("无法读取 turn-state 状态文件")
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
	m.pending = map[string]pending{}
	m.counters, m.last = Counters{}, Decision{}
	m.pruneLocked(m.now())
	return nil
}

func validateConfig(cfg *Config) error {
	if cfg.InjectMode != "always" && cfg.InjectMode != "replace-only" {
		return errors.New("inject_mode 只能是 replace-only 或 always")
	}
	if cfg.TemplateLength < 100 || cfg.TemplateLength > 8192 || cfg.ReplaceLength < 100 || cfg.ReplaceLength > 8192 || cfg.TemplateLength == cfg.ReplaceLength {
		return errors.New("模板和替换长度须为 100–8192 且不能相同")
	}
	if cfg.TTLSeconds < 60 || cfg.TTLSeconds > 3600 {
		return errors.New("模板有效期须为 60–3600 秒，不能超过上游令牌有效期")
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
		if *pool, err = cleanList(*pool, 2000); err != nil {
			return err
		}
		for _, proxy := range *pool {
			if err := validateProxy(proxy); err != nil {
				return err
			}
		}
	}
	return nil
}

func cleanList(values []string, limit int) ([]string, error) {
	if len(values) > limit {
		return nil, fmt.Errorf("配置列表最多允许 %d 项", limit)
	}
	result := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("配置项过长或包含控制字符")
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
		return errors.New("代理必须是完整 URL：协议://用户名:密码@主机:端口")
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return errors.New("代理协议仅支持 http、https、socks5、socks5h")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("代理端口必须为 1–65535")
		}
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("代理包含控制字符")
	}
	return nil
}

// Update accepts a partial JSON configuration. Omitted proxy pools keep their
// credentials; [] explicitly clears a pool. Masked status URLs cannot be saved.
func (m *Manager) Update(raw []byte) error {
	// Do not let a completed probe commit a result under a newer configuration.
	// Probe holds this same gate for the whole bounded curl call.
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := cloneConfig(m.state.Config)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return errors.New("turn-state 配置格式无效")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("turn-state 配置只能包含一个 JSON 对象")
	}
	if err := validateConfig(&cfg); err != nil {
		return err
	}
	for _, pool := range [][]string{cfg.ProbeProxies, cfg.ProbeProxiesRotating} {
		for _, proxy := range pool {
			if strings.Contains(proxy, "***") {
				return errors.New("请填写完整代理，保留原值时应省略代理字段")
			}
		}
	}
	old := m.state.Config
	oldTemplates := m.state.Templates
	m.state.Templates = make(map[string]Template, len(oldTemplates))
	for k, v := range oldTemplates {
		m.state.Templates[k] = v
	}
	m.state.Config = cfg
	m.pruneLocked(m.now())
	if err := m.persistLocked(); err != nil {
		m.state.Config = old
		m.state.Templates = oldTemplates
		return err
	}
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
	cfg := cloneConfig(m.state.Config)
	counts := map[string]int{"static": len(cfg.ProbeProxies), "rotating": len(cfg.ProbeProxiesRotating)}
	for _, pool := range [][]string{cfg.ProbeProxies, cfg.ProbeProxiesRotating} {
		for i, proxy := range pool {
			pool[i] = maskProxy(proxy)
		}
	}
	rows := []TemplateView{}
	for _, t := range m.state.Templates {
		rows = append(rows, TemplateView{Account: t.Account, Model: t.Model, IssuedAt: t.IssuedAt,
			ExpiresAt: t.IssuedAt.Add(time.Duration(cfg.TTLSeconds) * time.Second), Length: len(t.Value)})
	}
	sort.Slice(rows, func(i, j int) bool { return key(rows[i].Account, rows[i].Model) < key(rows[j].Account, rows[j].Model) })
	return Status{Config: cfg, Templates: rows, Counters: m.counters, LastDecision: m.last, ProxyCounts: counts}
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
		m.recordLocked("pass", "无法确认所选账号和上游模型", account, model, now)
		return nil, nil
	}
	if requestID != "" && len(m.pending) < 4096 {
		m.pending[requestID] = pending{Account: account, Model: model, At: now}
	}
	value := headerValue(headers)
	t, ok := m.state.Templates[key(account, model)]
	if !ok {
		m.recordLocked("pass", "该账号和模型没有有效模板", account, model, now)
		return nil, nil
	}
	if value == t.Value {
		m.recordLocked("pass", "请求已携带当前模板", account, model, now)
		return nil, nil
	}
	if m.state.Config.InjectMode == "replace-only" && len(value) != m.state.Config.ReplaceLength {
		m.recordLocked("pass", "replace-only 仅替换指定长度的请求头", account, model, now)
		return nil, nil
	}
	if m.state.Config.DryRun {
		m.recordLocked("dry_run", "模板可用；观察模式未修改请求", account, model, now)
		return nil, nil
	}
	m.recordLocked("inject", "已注入同一账号和模型的有效模板", account, model, now)
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
			m.recordLocked("skip", "没有可验证的 Codex 请求归属", account, model, now)
		}
		return nil
	} else {
		if account != "" && account != p.Account {
			m.recordLocked("skip", "响应账号与请求归属不一致", account, model, now)
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
	if !validBucket(account, model) {
		m.recordLocked("skip", "无法确认响应账号和上游模型", account, model, now)
		return nil
	}
	if len(value) != m.state.Config.TemplateLength {
		m.recordLocked("skip", "响应头长度不符合模板长度", account, model, now)
		return nil
	}
	timestamp, ok := issuedAt(value)
	if !ok || timestamp.After(now) || now.Sub(timestamp) >= time.Duration(m.state.Config.TTLSeconds)*time.Second {
		m.recordLocked("skip", "模板时间戳无效、超前或已过期", account, model, now)
		return nil
	}
	k := key(account, model)
	old, exists := m.state.Templates[k]
	if exists && !timestamp.After(old.IssuedAt) {
		return nil
	}
	m.state.Templates[k] = Template{Account: account, Model: model, Value: value, IssuedAt: timestamp}
	if err := m.persistLocked(); err != nil {
		if exists {
			m.state.Templates[k] = old
		} else {
			delete(m.state.Templates, k)
		}
		m.recordLocked("error", "无法保存模板", account, model, now)
		return err
	}
	m.recordLocked("harvest", "已保存有效模板", account, model, now)
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
		return errors.New("清理单个模板须同时填写账号和模型")
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
	m.last = Decision{Action: action, Reason: reason, Account: account, Model: model, At: now}
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
		return errors.New("turn-state 尚未配置状态文件")
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return errors.New("无法创建 turn-state 状态目录")
	}
	raw, err := json.Marshal(m.state)
	if err != nil {
		return errors.New("无法编码 turn-state 状态")
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".turn-state-*")
	if err != nil {
		return errors.New("无法创建 turn-state 临时文件")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法写入 turn-state 状态")
	}
	if err := os.Rename(f.Name(), m.path); err != nil {
		return errors.New("无法替换 turn-state 状态文件")
	}
	return nil
}
