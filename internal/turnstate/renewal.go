package turnstate

import "time"

func configRenewalLead(cfg Config) time.Duration {
	if cfg.RenewBeforeMinutes > 0 {
		return time.Duration(cfg.RenewBeforeMinutes) * time.Minute
	}
	return renewalLead(cfg.TTLSeconds)
}

func templateRenewAt(t Template, cfg Config) time.Time {
	return t.IssuedAt.Add(time.Duration(cfg.TTLSeconds)*time.Second - configRenewalLead(cfg))
}

// Only successful exits follow the renewal schedule. Failed exit and account
// cooldowns retain their original budgets when the operator changes settings.
// Legacy v0.0.5 cooldowns have no success marker and remain in force until their
// original expiry; guessing from their deadline could reset a failed exit.
func (m *Manager) rescheduleRenewalsLocked() {
	rescheduleRenewals(&m.state)
}

func rescheduleRenewals(state *diskState) {
	for k, c := range state.Cooldowns {
		if t, ok := state.Templates[c.RenewalBucket]; c.RenewalBucket != "" && ok {
			c.Until = templateRenewAt(t, state.Config)
			state.Cooldowns[k] = c
		}
	}
}
