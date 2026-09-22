package turnstate

import "time"

// BerserkConcurrency is the most probes berserk mode runs at the same time.
const BerserkConcurrency = 10

// ProbeConcurrency is how many probes the collector may run now: the
// configured ProbeParallel, or BerserkConcurrency while a selected bucket's
// template is in its last BerserkMinutes and has not been renewed yet.
func (m *Manager) ProbeConcurrency() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.state.Config
	parallel := min(max(1, cfg.ProbeParallel), BerserkConcurrency)
	if !cfg.Berserk || cfg.Suspended {
		return parallel
	}
	now := m.now()
	for _, t := range m.state.Templates {
		if !m.inScopeLocked(t.Account, t.Model) || !m.usableLocked(t, now) {
			continue
		}
		if inBerserkWindow(t, cfg, now) {
			return BerserkConcurrency
		}
	}
	return parallel
}

func inBerserkWindow(t Template, cfg Config, now time.Time) bool {
	return cfg.Berserk && t.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second).Sub(now) <= time.Duration(cfg.BerserkMinutes)*time.Minute
}

// InScope reports whether State handles this account and upstream model at
// all. Business traffic outside the selected accounts and models skips State.
func (m *Manager) InScope(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inScopeLocked(account, ModelName(model))
}

func (m *Manager) inScopeLocked(account, model string) bool {
	if !contains(m.state.Config.ProbeAccounts, account) {
		return false
	}
	for _, candidate := range m.state.Config.Models {
		if ModelName(candidate) == model {
			return true
		}
	}
	return false
}

// BreakoutReady reports whether a request whose API key groups have no usable
// account left may retry on this selected account, because it holds a valid
// template for the upstream model and injection is live.
func (m *Manager) BreakoutReady(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.state.Config
	if !cfg.BreakoutRetry || cfg.Suspended || !cfg.Enabled || cfg.DryRun {
		return false
	}
	model = ModelName(model)
	if !m.inScopeLocked(account, model) {
		return false
	}
	t, ok := m.state.Templates[key(account, model)]
	return ok && m.usableLocked(t, m.now())
}

// StaleBucket reports a selected account and model that injection would use
// but whose template is missing or expired. Observe and off modes never steer.
func (m *Manager) StaleBucket(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.state.Config
	model = ModelName(model)
	if cfg.Suspended || !cfg.Enabled || cfg.DryRun || !m.inScopeLocked(account, model) {
		return false
	}
	t, ok := m.state.Templates[key(account, model)]
	return !ok || !m.usableLocked(t, m.now())
}
