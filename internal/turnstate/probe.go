package turnstate

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

// Credential is supplied for one probe, stays in memory and is never persisted.
type Credential struct {
	AccessToken string
	AccountID   string
	// ProbeVerifyCompletion is an in-memory request option, never an OAuth
	// credential field and never persisted with host authentication files.
	ProbeVerifyCompletion bool
}

type ProbeResponse struct {
	Status int
	Value  string
	// ProxyFailure is set only by the transport after a connection or proxy
	// authentication failure, never by upstream account or quota responses.
	ProxyFailure bool
	// CompletionFailure is a bounded, fixed diagnostic from strict active
	// probing. An unsuccessful response body is not proof of a broken proxy.
	CompletionFailure string
}

type ProbeResult struct {
	Action              string           `json:"action"`
	Reason              string           `json:"reason"`
	ReasonMessage       messages.Message `json:"reason_message,omitzero"`
	Account             string           `json:"account,omitempty"`
	Model               string           `json:"model,omitempty"`
	Exit                string           `json:"exit,omitempty"`
	Status              int              `json:"status,omitempty"`
	Length              int              `json:"length,omitempty"`
	NextCheckAt         time.Time        `json:"next_check_at"`
	ProxyIndex          int              `json:"proxy_index,omitempty"`
	ProxyTotal          int              `json:"proxy_total,omitempty"`
	ProxyPool           string           `json:"proxy_pool,omitempty"`
	ProxyAttempt        int              `json:"proxy_attempt,omitempty"`
	ProxyAttemptLimit   int              `json:"proxy_attempt_limit"`
	ProxyDisposition    string           `json:"proxy_disposition,omitempty"`
	ProxyRemaining      int              `json:"proxy_remaining,omitempty"`
	ProxyConfigRevision string           `json:"proxy_config_revision,omitempty"`
}

type ProbeProgress struct {
	Active bool        `json:"active"`
	Result ProbeResult `json:"result"`
}

// ProbeStats are process-local observations, not a detached runner or a claim
// that a browser is still polling. The last result remains visible on reload.
type ProbeStats struct {
	Attempts  uint64    `json:"attempts"`
	Harvested uint64    `json:"harvested"`
	Degraded  uint64    `json:"degraded"`
	Failed    uint64    `json:"failed"`
	Unchanged uint64    `json:"unchanged"`
	Since     time.Time `json:"since"`
}

// ProbeProgress reports only the selected, reserved in-flight candidate. It
// performs no host access or network I/O and starts no background work.
func (m *Manager) ProbeProgress() ProbeProgress {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeProbe == nil {
		return ProbeProgress{}
	}
	return ProbeProgress{Active: true, Result: *m.activeProbe}
}

type probeCandidate struct {
	account, model, proxy, cooldownKey  string
	rotating                            bool
	index, total, attempt, attemptLimit int
}

// probeProxyCursor is shared by all account/model buckets. Only a digest and
// an index are persisted, never another copy of the proxy URL or credentials.
// The identity survives insertion/reordering; the index is a safe fallback for
// old or externally edited state. Empty cursors start at the first entry.
type probeProxyCursor struct {
	Next  string `json:"next,omitempty"`
	Index int    `json:"index,omitempty"`
}

func probeProxyCount(cfg Config) int {
	return max(1, len(cfg.ProbeProxies)+len(cfg.ProbeProxiesRotating))
}

func probeProxyAt(cfg Config, index int) (string, bool) {
	if index < len(cfg.ProbeProxies) {
		return cfg.ProbeProxies[index], false
	}
	index -= len(cfg.ProbeProxies)
	if index < len(cfg.ProbeProxiesRotating) {
		return cfg.ProbeProxiesRotating[index], true
	}
	return "", false // An intentionally empty pool uses the direct exit.
}

func probeProxyIdentity(cfg Config, index int) string {
	proxy, rotating := probeProxyAt(cfg, index)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%t\x00%s", rotating, proxy)))
	return fmt.Sprintf("%x", hash)
}

func probeProxyStart(cfg Config, cursor probeProxyCursor) int {
	total := probeProxyCount(cfg)
	index := cursor.Index
	if index < 0 || index >= total {
		index = 0
	}
	if cursor.Next == "" || probeProxyIdentity(cfg, index) == cursor.Next {
		return index
	}
	for offset := 0; offset < total; offset++ {
		if probeProxyIdentity(cfg, offset) == cursor.Next {
			return offset
		}
	}
	return index
}

func probeProxyCursorAt(cfg Config, index int) probeProxyCursor {
	if len(cfg.ProbeProxies)+len(cfg.ProbeProxiesRotating) == 0 {
		return probeProxyCursor{}
	}
	index %= probeProxyCount(cfg)
	return probeProxyCursor{Next: probeProxyIdentity(cfg, index), Index: index}
}

func reconcileProbeProxyCursor(previous, next Config, cursor probeProxyCursor) probeProxyCursor {
	if slices.Equal(previous.ProbeProxies, next.ProbeProxies) && slices.Equal(previous.ProbeProxiesRotating, next.ProbeProxiesRotating) {
		return cursor
	}
	if cursor.Next == "" || len(next.ProbeProxies)+len(next.ProbeProxiesRotating) == 0 {
		return probeProxyCursorAt(next, 0)
	}
	// Resolve the next surviving entry in the old cyclic order. A map contains
	// only digests and positions, keeping edits linear in the pool size.
	positions := make(map[string]int, probeProxyCount(next))
	for index := 0; index < probeProxyCount(next); index++ {
		positions[probeProxyIdentity(next, index)] = index
	}
	start, total := probeProxyStart(previous, cursor), probeProxyCount(previous)
	for offset := 0; offset < total; offset++ {
		if index, exists := positions[probeProxyIdentity(previous, (start+offset)%total)]; exists {
			return probeProxyCursorAt(next, index)
		}
	}
	return probeProxyCursorAt(next, 0)
}

func (c probeCandidate) progress() ProbeResult {
	pool := "static"
	// Only authenticated management progress/events receive the complete URL.
	// Persisted template provenance remains masked in finishProbe.
	exit := c.proxy
	if c.rotating {
		pool = "rotating"
	} else if c.proxy == "" {
		pool = "direct"
		exit = "direct"
	}
	return ProbeResult{Account: c.account, Model: c.model, Exit: exit,
		ProxyIndex: c.index, ProxyTotal: c.total, ProxyPool: pool, ProxyAttempt: c.attempt, ProxyAttemptLimit: c.attemptLimit}
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
	model = ModelName(model)
	if !m.probeMu.TryLock() {
		reason := "A probe request is already running"
		return ProbeResult{Action: "busy", Reason: reason, ReasonMessage: messages.Literal(reason), NextCheckAt: time.Now().Add(3 * time.Second)}, nil
	}
	defer m.probeMu.Unlock()
	// Finalize observations before releasing the configuration/probe gate.
	// Otherwise a waiting ConfigureWith can switch storage and clear counters,
	// then have this old request overwrite the new manager's last result.
	defer func() {
		if result.ReasonMessage.IsZero() {
			result.ReasonMessage = messages.Literal(result.Reason)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if result.Action != "busy" && err == nil {
			m.lastProbe = result
			switch result.Action {
			case "harvested":
				m.probeStats.Harvested++
			case "degraded":
				m.probeStats.Degraded++
			case "unchanged":
				m.probeStats.Unchanged++
			case "error":
				if result.Account != "" {
					m.probeStats.Failed++
				}
			}
		} else if err != nil {
			m.probeStats.Failed++
		}
	}()
	m.writerMu.Lock()
	m.mu.Lock()
	now := m.now()
	m.pruneLocked(now)
	next := cloneState(m.state)
	selector := Manager{state: next, lastMissingBucket: m.lastMissingBucket, lastRenewBucket: m.lastRenewBucket, renewalBurst: m.renewalBurst}
	// A large pool may require scanning many cooling exits. Scan an immutable
	// snapshot outside the business mutex; probeMu still fixes configuration
	// and writerMu fixes the persisted cooldown state for this selection.
	m.mu.Unlock()
	pruneState(&next, now)
	candidate, result, ok := selector.selectProbeLocked(account, model, now, available)
	m.mu.Lock()
	if !ok {
		m.mu.Unlock()
		m.writerMu.Unlock()
		return result, nil
	}
	if budget := probeBudget(next, now); budget.Exhausted {
		m.mu.Unlock()
		m.writerMu.Unlock()
		return ProbeResult{Action: "budget_wait", Reason: "The hourly probe budget is exhausted; collection resumes when earlier attempts leave the rolling hour", NextCheckAt: budget.ResumesAt}, nil
	}
	// Reserve before credential access and transport, together with the exit
	// cooldown. Failures and process interruption keep this conservative charge;
	// a failed atomic save sends no upstream request and consumes no budget.
	next.ProbeUsage = reserveProbeUsage(next.ProbeUsage, now)
	if candidate.rotating {
		budget := rotatingBudget(next, candidate.account, candidate.model, now)
		if !budget.Until.After(now) && next.Config.ProbeRotatingCooldownMinutes > 0 {
			budget = cooldown{Until: now.Add(time.Duration(next.Config.ProbeRotatingCooldownMinutes) * time.Minute)}
		}
		// A disabled window creates no artificial future wait. Preserve and
		// charge a still-active old window, so toggling the setting does not
		// reset known attempts if the operator later re-enables its limit.
		if budget.Until.After(now) {
			budget.Attempts = addProbeUsageCount(max(0, budget.Attempts), 1)
			next.Cooldowns[rotatingBudgetKey(candidate.account, candidate.model)] = budget
			next.Cooldowns[candidate.cooldownKey] = cooldown{Until: budget.Until, Attempts: 1}
		}
	} else if next.Config.ProbeStaticCooldownMinutes > 0 {
		next.Cooldowns[candidate.cooldownKey] = cooldown{Until: now.Add(time.Duration(next.Config.ProbeStaticCooldownMinutes) * time.Minute)}
	}
	// Persist the next exit with the reservation. Another bucket and a process
	// restart both continue after this attempt; a crash must not keep returning
	// to the first URL. Eligibility and retry budgets remain bucket-specific.
	next.ProxyCursor = probeProxyCursorAt(next.Config, candidate.index)
	if err := m.commitStateLocked(next, false, ""); err != nil {
		m.mu.Unlock()
		m.writerMu.Unlock()
		return ProbeResult{}, err
	}
	m.lastProbeBucket = key(candidate.account, candidate.model)
	if t, exists := m.state.Templates[m.lastProbeBucket]; exists && m.usableLocked(t, now) {
		m.lastRenewBucket = m.lastProbeBucket
		m.renewalBurst++
	} else {
		m.renewalBurst = 0
		m.lastMissingBucket = m.lastProbeBucket
	}
	progress := candidate.progress()
	progress.Action = "probing"
	m.activeProbe = &progress
	m.probeStats.Attempts++
	m.mu.Unlock()
	m.writerMu.Unlock()
	defer func() {
		m.mu.Lock()
		m.activeProbe = nil
		m.mu.Unlock()
	}()
	credential, err := fetch(candidate.account)
	if err != nil {
		reason := messages.New("Cannot read a valid Codex OAuth credential; this account is paused for %d minutes. Sign in again or check the account", next.Config.ProbeAccountCooldownMinutes)
		if next.Config.ProbeAccountCooldownMinutes == 0 {
			reason = messages.Literal("Cannot read a valid Codex OAuth credential; account cooldown is disabled. Sign in again or check the account")
		}
		result, err := m.finishProbe(candidate, ProbeResponse{}, reason.Text, time.Duration(next.Config.ProbeAccountCooldownMinutes)*time.Minute)
		if err == nil {
			result.ReasonMessage = reason
		}
		return result, err
	}
	credential.ProbeVerifyCompletion = next.Config.ProbeVerifyCompletion
	response, err := m.runProbe(credential, candidate.model, candidate.proxy)
	if err != nil {
		// Never return OAuth tokens or proxy credentials in transport errors.
		return m.finishProbe(candidate, response, "The probe connection failed or exceeded 25 seconds; check the proxy and the network", 2*time.Second)
	}
	return m.finishProbe(candidate, response, "", 2*time.Second)
}

func rotatingBudgetKey(account, model string) string {
	hash := sha256.Sum256([]byte(key(account, model)))
	return "rotating:" + fmt.Sprintf("%x", hash)
}

func rotatingAttemptLimit(cfg Config) int {
	if cfg.ProbeRotatingCooldownMinutes == 0 {
		return 0
	}
	return cfg.ProbeRotatingMaxAttempts
}

// A bucket has one retry budget across the entire rotating pool. Existing
// per-exit counters are conservatively combined when upgrading, so a restart
// or adding another URL cannot multiply the account's upstream attempts.
func rotatingBudget(state diskState, account, model string, now time.Time) cooldown {
	if budget, exists := state.Cooldowns[rotatingBudgetKey(account, model)]; exists {
		if budget.Until.After(now) {
			return budget
		}
		return cooldown{}
	}
	var budget cooldown
	for _, proxy := range state.Config.ProbeProxiesRotating {
		previous := state.Cooldowns[proxyKey(account, model, proxy, true)]
		if previous.RenewalBucket != "" || !previous.Until.After(now) {
			continue
		}
		budget.Attempts = addProbeUsageCount(budget.Attempts, max(0, previous.Attempts))
		if previous.Until.After(budget.Until) {
			budget.Until = previous.Until
		}
	}
	return budget
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
	total, start := probeProxyCount(cfg), probeProxyStart(cfg, m.state.ProxyCursor)
	earlier := func(at time.Time) {
		if at.After(now) && at.Before(result.NextCheckAt) {
			result.NextCheckAt = at
		}
	}
	var renewing, missing []probeCandidate
	fresh, eligible, pending, paused := 0, 0, 0, 0
	for _, selectedAccount := range accounts {
		if available != nil && !available(selectedAccount) {
			continue
		}
		eligible++
		for _, selectedModel := range models {
			template, hasTemplate := m.state.Templates[key(selectedAccount, selectedModel)]
			hasTemplate = hasTemplate && m.usableLocked(template, now)
			if hasTemplate {
				renewAt := templateRenewAt(template, cfg)
				if renewAt.After(now) {
					fresh++
					earlier(renewAt)
					continue
				}
			}
			pending++
			rest := m.state.Cooldowns[accountKey(selectedAccount)]
			if cfg.ProbeAccountCooldownMinutes == 0 {
				rest.Until = rest.PacingUntil
			}
			if rest.Until.After(now) {
				paused++
				earlier(rest.Until)
				continue
			}
			var candidate probeCandidate
			found := false
			var budget cooldown
			budgetLoaded := false
			for offset := 0; offset < total; offset++ {
				index := (start + offset) % total
				proxy, rotating := probeProxyAt(cfg, index)
				if rotating && !budgetLoaded {
					budget = rotatingBudget(m.state, selectedAccount, selectedModel, now)
					budgetLoaded = true
				}
				limit := rotatingAttemptLimit(cfg)
				if rotating && limit > 0 && budget.Until.After(now) && budget.Attempts >= limit {
					earlier(budget.Until)
					continue
				}
				id := proxyKey(selectedAccount, selectedModel, proxy, rotating)
				rest := m.state.Cooldowns[id]
				if rest.Until.After(now) && (rest.RenewalBucket != "" || !rotating && cfg.ProbeStaticCooldownMinutes > 0) {
					earlier(rest.Until)
					continue
				}
				attempt := 1
				if rotating {
					attempt = addProbeUsageCount(max(0, budget.Attempts), 1)
				}
				candidate = probeCandidate{account: selectedAccount, model: selectedModel, proxy: proxy, cooldownKey: id, rotating: rotating, index: index + 1, total: total, attempt: attempt}
				if rotating {
					candidate.attemptLimit = limit
				}
				found = true
				break
			}
			if found {
				if hasTemplate {
					renewing = append(renewing, candidate)
				} else {
					missing = append(missing, candidate)
				}
			}
		}
	}
	if available != nil && eligible == 0 {
		return probeCandidate{}, ProbeResult{Action: "error", Reason: "No eligible Codex OAuth accounts are available", NextCheckAt: now.Add(time.Minute)}, false
	}
	// Renew active business buckets first, with a bounded burst so a persistently
	// bad renewal cannot starve new buckets. Within each class every bucket gets
	// one attempt before an earlier bucket gets another, regardless of pool size.
	candidates, cursor := missing, m.lastMissingBucket
	if len(renewing) > 0 && (len(missing) == 0 || m.renewalBurst < 3) {
		candidates, cursor = renewing, m.lastRenewBucket
	}
	if len(candidates) > 0 {
		selected := 0
		// Compare against the configured order even when the prior bucket is now
		// fresh/cooling and therefore absent from this call's candidate set.
		order := make(map[string]int, len(accounts)*len(models))
		for ai, a := range accounts {
			for mi, md := range models {
				order[key(a, md)] = ai*len(models) + mi
			}
		}
		if previous, exists := order[cursor]; exists {
			for index, candidate := range candidates {
				if order[key(candidate.account, candidate.model)] > previous {
					selected = index
					break
				}
			}
		}
		return candidates[selected], ProbeResult{}, true
	}
	if fresh == eligible*len(models) {
		result.Action, result.Reason = "fresh", "All templates are fresh; probing resumes shortly before expiry"
	} else if pending > 0 && paused == pending {
		result.Action, result.Reason = "account_wait", "Pending buckets are waiting for account pauses; collection will resume automatically"
	} else if paused > 0 {
		result.Reason = "Pending buckets are waiting for account pauses or exit cooldowns; collection will resume automatically"
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
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	next := cloneState(m.state)
	pruneState(&next, now)
	result := c.progress()
	result.Status, result.Length, result.NextCheckAt = response.Status, len(response.Value), now.Add(2*time.Second)
	switch {
	case failure != "":
		result.Action, result.Reason = "error", failure
	case response.Status == 401 || response.Status == 403:
		// Enabled refusal pauses apply to every model and exit for the account.
		result.ReasonMessage = messages.New("The upstream returned HTTP %d; this account is paused for %d minutes. Check sign-in, permissions, and the exit before retrying", response.Status, next.Config.ProbeAccountCooldownMinutes)
		if next.Config.ProbeAccountCooldownMinutes == 0 {
			result.ReasonMessage = messages.New("The upstream returned HTTP %d; account cooldown is disabled. Check sign-in, permissions, and the exit before retrying", response.Status)
		}
		result.Action, result.Reason = "error", result.ReasonMessage.Text
		rest = time.Duration(next.Config.ProbeAccountCooldownMinutes) * time.Minute
	case response.Status == 429:
		result.ReasonMessage = messages.New("The upstream rate-limited this account; probes are paused for %d minutes", next.Config.ProbeAccountCooldownMinutes)
		if next.Config.ProbeAccountCooldownMinutes == 0 {
			result.ReasonMessage = messages.Literal("The upstream rate-limited this account; account cooldown is disabled")
		}
		result.Action, result.Reason = "error", result.ReasonMessage.Text
		rest = time.Duration(next.Config.ProbeAccountCooldownMinutes) * time.Minute
	case response.Status != 200:
		result.ReasonMessage = messages.New("The upstream returned HTTP %d; no template was harvested", response.Status)
		result.Action, result.Reason = "error", result.ReasonMessage.Text
	case response.CompletionFailure != "":
		result.Action, result.Reason = "error", response.CompletionFailure
	case len(response.Value) == next.Config.TemplateLength:
		issued, parsed := issuedAt(response.Value)
		incoming := Template{Account: c.account, Model: c.model, Value: response.Value, IssuedAt: issued, Source: "probe", Exit: maskProxy(c.proxy), HarvestedAt: now}
		if !parsed || !usableWithConfig(incoming, next.Config, now) {
			result.Action, result.Reason = "error", "The response length matches, but its Fernet timestamp is invalid, in the future, or expired"
			break
		}
		bucket := key(c.account, c.model)
		if previous, exists := next.Templates[bucket]; exists && usableWithConfig(previous, next.Config, now) && !issued.After(previous.IssuedAt) {
			result.Action, result.Reason = "unchanged", "The upstream returned the same or an older template; its original expiry was not extended"
			break
		}
		next.Templates[bucket] = incoming
		result.Action, result.Reason = "harvested", "A valid template was harvested and saved for this account and model"
	case len(response.Value) == next.Config.ReplaceLength:
		result.Action, result.Reason = "degraded", "A degraded-length state was received and not saved; the next probe follows the exit pool retry rules"
	default:
		result.Action, result.Reason = "error", "The response did not contain a turn-state of the configured template length"
	}
	// Zero disables the long failure pause, never the ordinary request interval.
	rest = max(rest, 2*time.Second)
	next.Cooldowns[accountKey(c.account)] = cooldown{Until: now.Add(rest), PacingUntil: now.Add(2 * time.Second)}
	if result.Action == "harvested" {
		bucket := key(c.account, c.model)
		next.Cooldowns[c.cooldownKey] = cooldown{Until: templateRenewAt(next.Templates[bucket], next.Config), RenewalBucket: bucket}
		if c.rotating {
			// A successful bucket starts a fresh attempt budget when its new
			// template is due. Failure budgets must not postpone short-TTL
			// renewals or preserve nine old failures after a successful tenth.
			next.Cooldowns[rotatingBudgetKey(c.account, c.model)] = cooldown{Until: templateRenewAt(next.Templates[bucket], next.Config), RenewalBucket: bucket}
		}
	}
	removed := discardProbeProxy(&next, c, response, &result)
	if err := m.commitStateLocked(next, removed, ""); err != nil {
		if response.Status == 401 || response.Status == 403 || response.Status == 429 {
			// Upstream refusals are safety observations, not a tentative admin
			// edit. Keep the account pause in memory even on a disk failure,
			// and retry its persistence through management synchronization.
			id := accountKey(c.account)
			pause := next.Cooldowns[id]
			if previous := m.state.Cooldowns[id]; previous.Until.After(pause.Until) {
				pause.Until = previous.Until
			}
			m.state.Cooldowns[id] = pause
			m.runtimeDirty = true
		}
		if removed {
			result.Action = "error"
			result.Reason = "Cannot save automatic proxy removal; the proxy was retained and probing will continue"
			result.ReasonMessage = messages.Literal(result.Reason)
			result.ProxyDisposition = "retained_persistence_error"
			result.ProxyRemaining = len(m.state.Config.ProbeProxies) + len(m.state.Config.ProbeProxiesRotating)
			return result, nil
		}
		return ProbeResult{}, err
	}
	if result.Action == "harvested" {
		m.recordLocked("harvest", "A valid template was saved", c.account, c.model, now)
	}
	if removed {
		m.configRevision++
		result.ProxyConfigRevision = m.configRevisionTokenLocked()
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
