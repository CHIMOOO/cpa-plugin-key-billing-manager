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

// bucketRefreshAt is the pending cookie refresh of a template, or zero. A
// template whose model is no longer first keeps no refresh.
func bucketRefreshAt(t Template, cfg Config) time.Time {
	if !refreshesCookies(cfg, t.Model) {
		return time.Time{}
	}
	return t.RefreshAt
}

// bucketRenewAt is when a bucket with this template is due again: its cookie
// refresh or its renewal, whichever comes first.
func bucketRenewAt(t Template, cfg Config) time.Time {
	renew := templateRenewAt(t, cfg)
	if refresh := bucketRefreshAt(t, cfg); !refresh.IsZero() && refresh.Before(renew) {
		return refresh
	}
	return renew
}

// refreshesCookies reports whether a successful probe of this model schedules
// a cookie refresh: only the first selected model does, as one per account
// keeps the jar fresh.
func refreshesCookies(cfg Config, model string) bool {
	return cfg.CookieRefreshSeconds > 0 && len(cfg.Models) > 0 && cfg.Models[0] == model
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
			c.Until = bucketRenewAt(t, state.Config)
			state.Cooldowns[k] = c
		}
	}
}
