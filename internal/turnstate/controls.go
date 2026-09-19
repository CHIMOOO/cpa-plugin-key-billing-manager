package turnstate

import (
	"strings"

	"cpa-key-billing/internal/messages"
)

// ClearCooldowns removes failure cooldowns only. Fresh templates and their
// renewal waits survive; clearing cannot itself send a request or spend quota.
// A targeted reset also clears the account-level rejection pause, which is
// shared by its models. Configuration changes and probes use the same gate.
func (m *Manager) ClearCooldowns(account, model string) (int, error) {
	account, model = strings.TrimSpace(account), ModelName(model)
	if (account == "") != (model == "") || (account != "" && !validBucket(account, model)) {
		return 0, messages.Errorf("Both account and model are required to clear a single bucket's cooldown")
	}
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneLocked(now)
	selected := map[string]bool{}
	if account != "" {
		selected[accountKey(account)] = true
		selected[proxyKey(account, model, "", false)] = true
		for _, proxy := range m.state.Config.ProbeProxies {
			selected[proxyKey(account, model, proxy, false)] = true
		}
		for _, proxy := range m.state.Config.ProbeProxiesRotating {
			selected[proxyKey(account, model, proxy, true)] = true
		}
	}
	old := m.state.Cooldowns
	next := make(map[string]cooldown, len(old))
	cleared := 0
	for id, value := range old {
		if (account == "" || selected[id]) && value.RenewalBucket == "" {
			cleared++
			continue
		}
		next[id] = value
	}
	if cleared == 0 {
		return 0, nil
	}
	m.state.Cooldowns = next
	if err := m.persistLocked(); err != nil {
		m.state.Cooldowns = old
		return 0, err
	}
	return cleared, nil
}

// StoragePath is exposed only to the authenticated persistence diagnostic.
// It contains no template or proxy secret.
func (m *Manager) StoragePath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.path
}
