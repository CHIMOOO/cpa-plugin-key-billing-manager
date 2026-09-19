package turnstate

import "net/http"

// HasProtectedAccounts does not depend on Enabled: disabling injection must
// not turn an enrolled account into an unprotected business credential.
func (m *Manager) HasProtectedAccounts() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.state.Config.ProbeAccounts) > 0
}

func (m *Manager) AccountProtected(account string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return contains(m.state.Config.ProbeAccounts, account)
}

// BusinessReady is a conservative scheduler filter. The final interceptor
// repeats validation with the exact upstream model and current request header.
func (m *Manager) BusinessReady(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !contains(m.state.Config.ProbeAccounts, account) {
		return true
	}
	if !m.state.Config.Enabled || m.state.Config.DryRun {
		return false
	}
	t, ok := m.state.Templates[key(account, ModelName(model))]
	return ok && m.usableLocked(t, m.now())
}

// BeforeRequired validates and injects under one lock, so expiration or a
// concurrent configuration edit cannot occur between the safety check and
// header selection. Empty reason means the request may proceed.
func (m *Manager) BeforeRequired(requestID, account, model string, headers http.Header) (http.Header, []string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !contains(m.state.Config.ProbeAccounts, account) {
		updated, clear := m.beforeLocked(requestID, account, model, headers)
		return updated, clear, ""
	}
	now := m.now()
	model = ModelName(model)
	if !m.state.Config.Enabled || m.state.Config.DryRun {
		return nil, nil, "Turn State injection is disabled or observing; this protected account cannot serve business requests"
	}
	t, ok := m.state.Templates[key(account, model)]
	if !ok || !m.usableLocked(t, now) {
		return nil, nil, "This protected account has no current Turn State template for the selected upstream model"
	}
	value := headerValue(headers)
	if value != t.Value && m.state.Config.InjectMode == "replace-only" && len(value) != m.state.Config.ReplaceLength {
		return nil, nil, "replace-only cannot inject this request header; use always or supply a replaceable header"
	}
	updated, clear := m.beforeAtLocked(requestID, account, model, headers, now)
	return updated, clear, ""
}
