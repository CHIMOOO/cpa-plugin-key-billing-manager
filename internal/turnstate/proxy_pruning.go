package turnstate

// discardProbeProxyLocked applies the optional policy to the exact configured
// URL and pool used by this probe. The caller holds probeMu and mu and commits
// the pool and cooldown changes in the same persistence transaction.
func (m *Manager) discardProbeProxyLocked(c probeCandidate, response ProbeResponse, result *ProbeResult) bool {
	cfg := &m.state.Config
	failed := response.ProxyFailure && (response.Status == 0 || response.Status == 407)
	if c.proxy == "" || !(cfg.ProbeDropFailedProxies && failed || cfg.ProbeDropDegradedProxies && result.Action == "degraded") {
		return false
	}
	pool := &cfg.ProbeProxies
	if c.rotating {
		pool = &cfg.ProbeProxiesRotating
	}
	for index, value := range *pool {
		if value != c.proxy {
			continue
		}
		remaining := len(cfg.ProbeProxies) + len(cfg.ProbeProxiesRotating)
		// Always retain at least one explicit proxy, even if invalid in-memory
		// configuration is supplied. Automatic deletion must never select the
		// direct fallback that is used only for intentionally empty pools.
		minimum := max(1, cfg.ProbeMinProxies)
		if remaining <= minimum {
			result.ProxyDisposition, result.ProxyRemaining = "retained_minimum", remaining
			return false
		}
		// Do not mutate the old slice: finishProbe needs it intact to roll back
		// a failed disk commit. Session credentials distinguish entries sharing
		// one gateway, and the other pool is deliberately untouched.
		updated := make([]string, 0, len(*pool)-1)
		updated = append(updated, (*pool)[:index]...)
		updated = append(updated, (*pool)[index+1:]...)
		*pool = updated
		result.ProxyDisposition, result.ProxyRemaining = "removed", remaining-1
		return true
	}
	return false
}
