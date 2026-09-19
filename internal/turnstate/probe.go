package turnstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

// Credential is supplied for one probe, stays in memory and is never persisted.
type Credential struct {
	AccessToken string
	AccountID   string
}

type ProbeResponse struct {
	Status int
	Value  string
}

type ProbeResult struct {
	Action        string           `json:"action"`
	Reason        string           `json:"reason"`
	ReasonMessage messages.Message `json:"reason_message,omitzero"`
	Account       string           `json:"account,omitempty"`
	Model         string           `json:"model,omitempty"`
	Exit          string           `json:"exit,omitempty"`
	Status        int              `json:"status,omitempty"`
	Length        int              `json:"length,omitempty"`
	NextCheckAt   time.Time        `json:"next_check_at"`
}

type probeCandidate struct {
	account, model, proxy, cooldownKey string
	rotating                           bool
}

func proxyKey(account, model, proxy string, rotating bool) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%t", account, model, proxy, rotating)))
	return "exit:" + fmt.Sprintf("%x", h)
}

func accountKey(account string) string {
	h := sha256.Sum256([]byte(account))
	return "account:" + fmt.Sprintf("%x", h)
}

// Probe performs at most one upstream request. The browser may call it again;
// there is no detached runner, goroutine or timer after this method returns.
// fetch must return only the specifically selected OAuth credential.
func (m *Manager) Probe(account, model string, fetch func(string) (Credential, error)) (ProbeResult, error) {
	return m.probe(account, model, nil, fetch)
}

// ProbeWithAvailability is Probe with a host-supplied snapshot of accounts
// eligible for explicit probes, independently of business-routing enablement.
// It prevents deleted or unsupported accounts from consuming a cooldown bucket
// before fetch is called.
func (m *Manager) ProbeWithAvailability(account, model string, available func(string) bool, fetch func(string) (Credential, error)) (ProbeResult, error) {
	return m.probe(account, model, available, fetch)
}

func (m *Manager) probe(account, model string, available func(string) bool, fetch func(string) (Credential, error)) (result ProbeResult, err error) {
	defer func() {
		if result.ReasonMessage.IsZero() {
			result.ReasonMessage = messages.Literal(result.Reason)
		}
	}()
	model = ModelName(model)
	if !m.probeMu.TryLock() {
		return ProbeResult{Action: "busy", Reason: "A probe request is already running", NextCheckAt: time.Now().Add(3 * time.Second)}, nil
	}
	defer m.probeMu.Unlock()
	m.mu.Lock()
	now := m.now()
	m.pruneLocked(now)
	candidate, result, ok := m.selectProbeLocked(account, model, now, available)
	if !ok {
		m.mu.Unlock()
		return result, nil
	}
	old, existed := m.state.Cooldowns[candidate.cooldownKey]
	reserved := cooldown{Until: now.Add(55 * time.Minute)}
	if candidate.rotating {
		reserved = cooldown{Until: now.Add(10 * time.Minute), Attempts: old.Attempts + 1}
	}
	m.state.Cooldowns[candidate.cooldownKey] = reserved
	if err := m.persistLocked(); err != nil {
		if existed {
			m.state.Cooldowns[candidate.cooldownKey] = old
		} else {
			delete(m.state.Cooldowns, candidate.cooldownKey)
		}
		m.mu.Unlock()
		return ProbeResult{}, err
	}
	m.mu.Unlock()
	credential, err := fetch(candidate.account)
	if err != nil {
		return m.finishProbe(candidate, ProbeResponse{}, "Cannot read a valid Codex OAuth credential; sign in again or check the account", 10*time.Minute)
	}
	response, err := m.runProbe(credential, candidate.model, candidate.proxy)
	if err != nil {
		// Never return OAuth tokens or proxy credentials in transport errors.
		return m.finishProbe(candidate, response, "The probe connection failed or exceeded 25 seconds; check the proxy and the network", 2*time.Second)
	}
	return m.finishProbe(candidate, response, "", 2*time.Second)
}

func (m *Manager) selectProbeLocked(account, model string, now time.Time, available func(string) bool) (probeCandidate, ProbeResult, bool) {
	cfg := m.state.Config
	result := ProbeResult{Action: "cooling", Reason: "All exits for pending account/model buckets are cooling down", NextCheckAt: now.Add(time.Hour)}
	accounts, models := cfg.ProbeAccounts, cfg.Models
	if account != "" {
		if !contains(accounts, account) {
			return probeCandidate{}, ProbeResult{Action: "error", Reason: "The account is outside the saved probe scope", NextCheckAt: now.Add(time.Minute)}, false
		}
		accounts = []string{account}
	}
	if model != "" {
		if !contains(models, model) {
			return probeCandidate{}, ProbeResult{Action: "error", Reason: "The model is outside the saved probe scope", NextCheckAt: now.Add(time.Minute)}, false
		}
		models = []string{model}
	}
	if len(accounts) == 0 || len(models) == 0 {
		return probeCandidate{}, ProbeResult{Action: "error", Reason: "Save the probe accounts and models first", NextCheckAt: now.Add(time.Minute)}, false
	}
	statics := cfg.ProbeProxies
	if len(statics) == 0 && len(cfg.ProbeProxiesRotating) == 0 {
		statics = []string{""}
	}
	fresh := 0
	availableAccounts := 0
	for _, a := range accounts {
		if available != nil && !available(a) {
			continue
		}
		availableAccounts++
		for _, model := range models {
			if t, ok := m.state.Templates[key(a, model)]; ok {
				renewAt := t.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second - renewalLead(cfg.TTLSeconds))
				if renewAt.After(now) {
					fresh++
					if renewAt.Before(result.NextCheckAt) {
						result.NextCheckAt = renewAt
					}
					continue
				}
			}
			if c := m.state.Cooldowns[accountKey(a)]; c.Until.After(now) {
				if c.Until.Before(result.NextCheckAt) {
					result.NextCheckAt = c.Until
				}
				continue
			}
			for poolIndex, pool := range [][]string{statics, cfg.ProbeProxiesRotating} {
				for _, proxy := range pool {
					rotating := poolIndex == 1
					k := proxyKey(a, model, proxy, rotating)
					c := m.state.Cooldowns[k]
					if c.Until.After(now) && (!rotating || c.Attempts >= 10) {
						if c.Until.Before(result.NextCheckAt) {
							result.NextCheckAt = c.Until
						}
						continue
					}
					return probeCandidate{account: a, model: model, proxy: proxy, cooldownKey: k, rotating: rotating}, ProbeResult{}, true
				}
			}
		}
	}
	if available != nil && availableAccounts == 0 {
		return probeCandidate{}, ProbeResult{Action: "error", Reason: "No eligible Codex OAuth accounts are available", NextCheckAt: now.Add(time.Minute)}, false
	}
	if fresh == availableAccounts*len(models) {
		result.Action, result.Reason = "fresh", "All templates are fresh; probing resumes shortly before expiry"
	}
	return probeCandidate{}, result, false
}

func renewalLead(ttl int) time.Duration {
	seconds := ttl / 4
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (m *Manager) finishProbe(c probeCandidate, response ProbeResponse, failure string, rest time.Duration) (ProbeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	result := ProbeResult{Account: c.account, Model: c.model, Exit: maskProxy(c.proxy), Status: response.Status,
		Length: len(response.Value), NextCheckAt: now.Add(2 * time.Second)}
	switch {
	case failure != "":
		result.Action, result.Reason = "error", failure
	case response.Status == 401 || response.Status == 403:
		result.Action, result.Reason = "error", "The upstream rejected this account; probes are paused for 55 minutes. Check sign-in and permissions"
		rest = 55 * time.Minute
	case response.Status == 429:
		result.Action, result.Reason = "error", "The upstream rate-limited this account; probes are paused for 10 minutes"
		rest = 10 * time.Minute
	case response.Status != 200:
		result.ReasonMessage = messages.New("The upstream returned HTTP %d; no template was harvested", response.Status)
		result.Action, result.Reason = "error", result.ReasonMessage.Text
	case len(response.Value) == m.state.Config.TemplateLength:
		issued, parsed := issuedAt(response.Value)
		incoming := Template{Account: c.account, Model: c.model, Value: response.Value, IssuedAt: issued}
		if !parsed || !m.usableLocked(incoming, now) {
			result.Action, result.Reason = "error", "The response length matches, but its Fernet timestamp is invalid, in the future, or expired"
			break
		}
		if previous, exists := m.state.Templates[key(c.account, c.model)]; exists && m.usableLocked(previous, now) && !issued.After(previous.IssuedAt) {
			result.Action, result.Reason = "unchanged", "The upstream returned the same or an older template; its original expiry was not extended"
			break
		}
		if err := m.learnLocked(c.account, c.model, response.Value, now); err != nil {
			return ProbeResult{}, err
		}
		if t, ok := m.state.Templates[key(c.account, c.model)]; ok && t.Value == response.Value && t.IssuedAt.Equal(issued) && m.usableLocked(t, now) {
			result.Action, result.Reason = "harvested", "A valid template was harvested and saved for this account and model"
		} else {
			result.Action, result.Reason = "error", "The response template was not saved or renewed"
		}
	case len(response.Value) == m.state.Config.ReplaceLength:
		result.Action, result.Reason = "degraded", "A degraded-length state was received and not saved; the next probe follows the exit pool retry rules"
	default:
		result.Action, result.Reason = "error", "The response did not contain a turn-state of the configured template length"
	}
	accountCooldownKey := accountKey(c.account)
	oldCooldown, hadCooldown := m.state.Cooldowns[accountCooldownKey]
	m.state.Cooldowns[accountCooldownKey] = cooldown{Until: now.Add(rest)}
	oldExitCooldown := m.state.Cooldowns[c.cooldownKey]
	if result.Action == "harvested" {
		// A successful exit may be reused when its template needs renewal.
		// Keeping the failure budget of 55 minutes would outlive short TTLs,
		// and even the default TTL when the returned template is already old.
		t := m.state.Templates[key(c.account, c.model)]
		renewAt := t.IssuedAt.Add(time.Duration(m.state.Config.TTLSeconds)*time.Second - renewalLead(m.state.Config.TTLSeconds))
		m.state.Cooldowns[c.cooldownKey] = cooldown{Until: renewAt}
	}
	// next_check_at is a global scheduling hint, not necessarily this account's
	// cooldown: another saved account can be probed by the next bounded call.
	if err := m.persistLocked(); err != nil {
		m.state.Cooldowns[c.cooldownKey] = oldExitCooldown
		if hadCooldown {
			m.state.Cooldowns[accountCooldownKey] = oldCooldown
		} else {
			delete(m.state.Cooldowns, accountCooldownKey)
		}
		return ProbeResult{}, err
	}
	return result, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ParseCredential reads only OAuth access tokens. API keys are not supported;
// the probe never refreshes or writes credentials back to the host.
func ParseCredential(raw []byte, now time.Time) (Credential, error) {
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		return Credential{}, messages.Errorf("Invalid authentication file format")
	}
	token, _ := file["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return Credential{}, messages.Errorf("The authentication file has no OAuth access_token")
	}
	account, _ := file["account_id"].(string)
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		if json.Unmarshal(claimsRaw, &claims) == nil {
			if expiration, ok := claims["exp"].(float64); ok && expiration <= float64(now.Unix()) {
				return Credential{}, messages.Errorf("The OAuth access_token has expired; wait for CPA to refresh it or sign in again")
			}
			if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
				if id, _ := auth["chatgpt_account_id"].(string); id != "" {
					account = id
				}
			}
		}
	}
	if strings.ContainsAny(account, "\r\n\x00") {
		return Credential{}, messages.Errorf("Invalid account in the authentication file")
	}
	return Credential{AccessToken: token, AccountID: account}, nil
}
