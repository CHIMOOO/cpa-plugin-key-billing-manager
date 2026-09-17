package plugin

import "cpa-key-billing/internal/billing"

// Check the exact credential selected by CPA as well as the scheduler's candidate
// subset. Other host scheduling modes may skip this plugin's scheduler entirely.
// Classification comes only from host inventory keyed by the same fingerprint;
// unknown source/provider metadata cannot grant a provider-wide permission.
func (a *App) enforceSelectedCredential(req RequestInterceptRequest) RequestInterceptResponse {
	if a == nil || a.store == nil || !a.store.Enabled() ||
		metadataString(req.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return RequestInterceptResponse{}
	}
	decision := a.store.ResolveRouting(metadataString(req.Metadata, MetadataCallerScope), req.Model, req.RequestedModel)
	if decision.AccessDenied != "" {
		return accessDeniedResponse(req.SourceFormat, decision.AccessDenied)
	}
	if decision.ConfigurationError != "" {
		return routingConfigurationResponse(req.SourceFormat, decision.ConfigurationError)
	}
	if !decision.RestrictsCredentials() {
		return RequestInterceptResponse{}
	}
	id := metadataString(req.Metadata, MetadataSelectedAuth)
	if id == "" {
		return accessDeniedResponse(req.SourceFormat, "无法确认 CPA 选中的上游凭证，访问已被禁止")
	}
	ref := billing.CredentialFingerprint(id)
	a.routingMu.Lock()
	credential := a.credentials[ref]
	a.routingMu.Unlock()
	if !decision.AllowsCredential(ref, credential.Source, credential.Provider) {
		return accessDeniedResponse(req.SourceFormat, "CPA 选中的上游凭证不在允许范围内，访问已被禁止")
	}
	return RequestInterceptResponse{}
}
