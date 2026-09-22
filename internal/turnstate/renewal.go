package turnstate

import (
	"slices"
	"time"
)

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

// refreshRoundLimit is the most exits one cookie refresh round tries.
func refreshRoundLimit(cfg Config) int {
	return min(25, probeProxyCount(cfg))
}

// refreshRound returns the exits a refresh round has tried once this exit
// is tried too. Trying a tried exit again means the round started over.
func refreshRound(tried []string, exit string) []string {
	if slices.Contains(tried, exit) {
		return []string{exit}
	}
	return append(slices.Clone(tried), exit)
}

// continueRefreshRoundLocked handles a cookie refresh without a 292, like the
// reference's collection round: the round moves on promptly to an exit it has
// not tried (the account pacing still spaces the attempts) until it has tried
// refreshRoundLimit exits or none is left, and the next round then waits
// CookieRetrySeconds. A 312 or another upstream answer never cools the exit;
// only a failure of the exit itself starts its static cooldown. Like
// commitStateLocked, it briefly releases mu: next is private and writerMu
// keeps other commits out while a large pool is scanned.
func (m *Manager) continueRefreshRoundLocked(next *diskState, c probeCandidate, exitFailed bool, now time.Time) {
	cfg := next.Config
	if exitFailed && !c.rotating && cfg.ProbeStaticCooldownMinutes > 0 {
		next.Cooldowns[c.cooldownKey] = cooldown{Until: now.Add(time.Duration(cfg.ProbeStaticCooldownMinutes) * time.Minute)}
	}
	bucket := key(c.account, c.model)
	t, ok := next.Templates[bucket]
	if !ok {
		return
	}
	tried := refreshRound(t.RefreshTried, c.cooldownKey)
	t.RefreshAt, t.RefreshTried = time.Time{}, nil
	if refreshesCookies(cfg, c.model) {
		t.RefreshAt = now.Add(time.Duration(cfg.CookieRetrySeconds) * time.Second)
		if len(tried) < refreshRoundLimit(cfg) {
			m.mu.Unlock()
			left := refreshExitLeft(*next, c.account, c.model, tried, now)
			m.mu.Lock()
			if left {
				t.RefreshAt, t.RefreshTried = now, tried
			}
		}
	}
	next.Templates[bucket] = t
}

// refreshExitLeft reports whether a refresh round has an exit it has not
// tried that the selector would use now for this bucket.
func refreshExitLeft(state diskState, account, model string, tried []string, now time.Time) bool {
	cfg := state.Config
	budget := rotatingBudget(state, account, model, now)
	for index := range probeProxyCount(cfg) {
		proxy, rotating := probeProxyAt(cfg, index)
		id := proxyKey(account, model, proxy, rotating)
		if !slices.Contains(tried, id) && !(rotating && rotatingExhausted(cfg, budget, now)) && !exitCooling(cfg, state.Cooldowns[id], rotating, now) {
			return true
		}
	}
	return false
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
