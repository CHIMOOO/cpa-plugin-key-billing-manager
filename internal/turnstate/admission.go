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

// Protects reports whether a request must pass the State gate: a selected
// account serving a selected model. An unknown model cannot prove it is out of
// scope, so it stays protected.
func (m *Manager) Protects(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.protectsLocked(account, ModelName(model))
}

func (m *Manager) protectsLocked(account, model string) bool {
	return contains(m.state.Config.ProbeAccounts, account) && (model == "" || m.inScopeLocked(account, model))
}

// BusinessReady is a conservative scheduler filter. The final interceptor
// repeats validation with the exact upstream model and current request header.
// Models outside the saved scope never need a template, even on a selected
// account.
func (m *Manager) BusinessReady(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.protectsLocked(account, ModelName(model)) {
		return true
	}
	if !m.state.Config.Enabled || m.state.Config.DryRun {
		return false
	}
	t, ok := m.state.Templates[key(account, ModelName(model))]
	return ok && m.usableLocked(t, m.now())
}

// TemplateReady reports whether the account holds a usable template for
// exactly this model, whatever the saved scope.
func (m *Manager) TemplateReady(account, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if !m.protectsLocked(account, model) {
		// A selected account serving a model outside the scope skips State.
		return nil, nil, ""
	}
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
