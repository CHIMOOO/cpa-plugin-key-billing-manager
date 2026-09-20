package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

const (
	managementBase = "/v0/management/plugins/" + PluginID
	resourceBase   = "/v0/resource/plugins/" + PluginID
	resourceUIPath = "/ui"
)

const (
	routeKeys                   = "/keys"
	routeCredentials            = "/credentials"
	routeProfile                = "/profile"
	routeSubscription           = "/subscription"
	routeRouting                = "/routing"
	routePrices                 = "/prices"
	routeReferencePrices        = "/prices/reference"
	routeReferencePricesStatus  = "/prices/reference/status"
	routeReferencePricesRefresh = "/prices/reference/refresh"
	routePlans                  = "/plans"
	routeRoutes                 = "/routes"
	routeGroups                 = "/groups"
	routeKeysGroups             = "/keys/groups"
	routeAccessControl          = "/access-control"
	routeTurnState              = "/turn-state"
	routeTurnStateTemplates     = "/turn-state/templates"
	routeTurnStateDiscard       = "/turn-state/templates/discard"
	routeTurnStateProbe         = "/turn-state/probe"
	routeTurnStateRunner        = "/turn-state/runner"
	routeTurnStateRunnerTick    = "/turn-state/runner/tick"
	routeTurnStateUpload        = "/turn-state/config-upload"
	routeTurnStateUploadCommit  = "/turn-state/config-upload/commit"
	routeTurnStateProxiesRead   = "/turn-state/proxies/read"
	routeTurnStateProxiesTest   = "/turn-state/proxies/test"
	routeTurnStateProbeProgress = "/turn-state/probe-progress"
	routeTurnStateCooldowns     = "/turn-state/cooldowns/clear"
	routeTurnStateSelfTest      = "/turn-state/self-test"
	routePersistence            = "/persistence"
	routeKeysRoutes             = "/keys/routes"
	routeKeysBind               = "/keys/bind"
	routeKeysUnbind             = "/keys/unbind"
	routeKeysReset              = "/keys/reset"
	routeKeysLabel              = "/keys/label"
	routeKeysConcurrency        = "/keys/concurrency"
	routeKeysSync               = "/keys/sync"
	routeCredentialsSync        = "/credentials/sync"
	routeEvents                 = "/events"
	routeEventKeys              = "/events/keys"
	routeErrors                 = "/errors"
	routeAnalysis               = "/analysis"
	routePluginLogs             = "/plugin-logs"
	routeAuthFiles              = "/auth-files"
	routeAuthQuota              = "/auth-files/quota"
	routeIntegrations           = "/integrations"
	routeIntegrationsQuery      = "/integrations/query"
	routeIntegrationsChannel    = "/integrations/channel"
	routeIntegrationsDevice     = "/integrations/cline/device"
	routeIntegrationsPoll       = "/integrations/cline/poll"
	routeIntegrationsRefresh    = "/integrations/refresh"
	routeIntegrationsCommit     = "/integrations/commit"
	routeIntegrationsCancel     = "/integrations/cline/cancel"
)

type managementEndpoint struct {
	method, path, description string
	handle                    func(*App, ManagementRequest) ManagementResponse
}

var managementEndpoints = []managementEndpoint{
	{http.MethodGet, "/model-tests", "View model test accounts and original test presets", (*App).getModelTests},
	{http.MethodPost, "/model-tests/prepare", "Prepare one explicitly proxied account model test", (*App).prepareModelTest},
	{http.MethodPost, "/model-tests/complete", "Release a model test and evaluate its displayed output", (*App).completeModelTest},
	{http.MethodGet, "/account-runtime", "View upstream account usage and concurrency", (*App).getAccountRuntime},
	{http.MethodGet, "/account-runtime/settings", "View account runtime settings", (*App).getAccountRuntimeSettings},
	{http.MethodPut, "/account-runtime/settings", "Save account runtime settings", (*App).setAccountRuntimeSettings},
	{http.MethodGet, "/risk-center", "View content risk policies and redacted events", (*App).getRiskCenter},
	{http.MethodPut, "/risk-center/config", "Save content risk policy", (*App).setRiskConfig},
	{http.MethodDelete, "/risk-center/events", "Clear redacted content risk events", (*App).clearRiskEvents},
	{http.MethodDelete, "/risk-center/hashes", "Clear content risk hash memory", (*App).clearRiskHashes},
	{http.MethodGet, routeKeys, "View API key status", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"keys": a.keyRows()})
	}},
	{http.MethodGet, routeGroups, "View API key groups", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"groups": a.groupRows()})
	}},
	{http.MethodGet, routeAccessControl, "View access control settings", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"access_control": a.store.AccessControl()})
	}},
	{http.MethodPut, routeAccessControl, "Save access control settings", (*App).setAccessControl},
	{http.MethodGet, routeTurnState, "View Codex turn-state settings and template status", (*App).getTurnState},
	{http.MethodPut, routeTurnState, "Save Codex turn-state settings", (*App).setTurnState},
	{http.MethodPost, routeTurnStateUpload, "Begin a staged Codex turn-state settings upload", (*App).beginTurnStateUpload},
	{http.MethodPatch, routeTurnStateUpload, "Stage a Codex turn-state settings chunk", (*App).appendTurnStateUpload},
	{http.MethodDelete, routeTurnStateUpload, "Cancel a staged Codex turn-state settings upload", (*App).cancelTurnStateUpload},
	{http.MethodPost, routeTurnStateUploadCommit, "Apply complete staged Codex turn-state settings", (*App).commitTurnStateUpload},
	{http.MethodDelete, routeTurnStateTemplates, "Clear Codex turn-state templates", (*App).clearTurnState},
	{http.MethodPost, routeTurnStateDiscard, "Discard one exact Codex turn-state template", (*App).discardTurnState},
	{http.MethodPost, routeTurnStateProbe, "Run one Codex turn-state probe", (*App).probeTurnState},
	{http.MethodGet, routeTurnStateRunner, "View server collection status and recent probe events", (*App).getTurnStateRunner},
	{http.MethodPut, routeTurnStateRunner, "Start or stop server collection durably", (*App).setTurnStateRunner},
	{http.MethodPost, routeTurnStateRunnerTick, "Run one due server collection step under the collector lease", (*App).tickTurnStateRunner},
	{http.MethodPost, routeTurnStateProxiesRead, "Read a page of saved proxy URLs for editing", (*App).readTurnStateProxies},
	{http.MethodPost, routeTurnStateProxiesTest, "Test one proxy connection without using an account", (*App).testTurnStateProxy},
	{http.MethodGet, routeTurnStateProbeProgress, "View the active Codex turn-state probe exit", (*App).getTurnStateProbeProgress},
	{http.MethodPost, routeTurnStateCooldowns, "Clear failed Codex turn-state probe cooldowns", (*App).clearTurnStateCooldowns},
	{http.MethodPost, routeTurnStateSelfTest, "Test a specific Codex account and model without harvesting", (*App).selfTestTurnState},
	{http.MethodGet, routePersistence, "Inspect plugin storage mounts when detectable", (*App).getPersistence},
	{http.MethodPost, routeGroups, "Create API key group", (*App).createGroup},
	{http.MethodPatch, routeGroups, "Update API key group", (*App).updateGroup},
	{http.MethodDelete, routeGroups, "Delete API key group", (*App).deleteGroup},
	{http.MethodPut, routeKeysGroups, "Replace API key group bindings", (*App).setKeyGroups},
	{http.MethodGet, routePlans, "View subscription plans", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"plans": a.store.Plans()})
	}},
	{http.MethodGet, routeRoutes, "View routing rules", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"routes": a.routeRows()})
	}},
	{http.MethodGet, routeCredentials, "View routing credential options", (*App).listCredentials},
	{http.MethodGet, routePrices, "View model pricing", func(a *App, req ManagementRequest) ManagementResponse { return a.listPrices(req, viewAccess{}) }},
	{http.MethodGet, routeReferencePrices, "Search model reference prices", (*App).searchReferencePrices},
	{http.MethodGet, routeReferencePricesStatus, "View reference price status", func(a *App, _ ManagementRequest) ManagementResponse { return a.referencePriceStatus() }},
	{http.MethodPost, routeReferencePricesRefresh, "Update reference prices", func(a *App, _ ManagementRequest) ManagementResponse { return a.refreshReferencePrices() }},
	{http.MethodDelete, routePrices, "Delete custom model pricing", (*App).deletePrice},
	{http.MethodPut, routePrices, "Update model pricing", (*App).putPrices},
	{http.MethodPost, routePlans, "Create subscription plan", (*App).createPlan},
	{http.MethodPatch, routePlans, "Update subscription plan", (*App).updatePlan},
	{http.MethodDelete, routePlans, "Delete subscription plan and unbind API keys", (*App).deletePlan},
	{http.MethodPost, routeRoutes, "Create routing rule", (*App).createRoute},
	{http.MethodPatch, routeRoutes, "Update routing rule", (*App).updateRoute},
	{http.MethodDelete, routeRoutes, "Delete routing rule and remove its bindings", (*App).deleteRoute},
	{http.MethodPut, routeKeysRoutes, "Update API key routing bindings", (*App).setKeyRoutes},
	{http.MethodPost, routeKeysBind, "Bind API key to subscription plan", (*App).bindKey},
	{http.MethodPost, routeKeysUnbind, "Unbind API key from subscription plan", (*App).unbindKey},
	{http.MethodPost, routeKeysReset, "Reset subscription quotas for selected API keys", (*App).resetKeys},
	{http.MethodPost, routeKeysLabel, "Set API key label", (*App).labelKey},
	{http.MethodPost, routeKeysConcurrency, "Set API key concurrency limit", (*App).setKeyConcurrency},
	{http.MethodPost, routeKeysSync, "Sync API keys from CLIProxyAPI", (*App).syncKeys},
	{http.MethodPost, routeCredentialsSync, "Sync configured credentials", (*App).syncConfiguredCredentials},
	{http.MethodGet, routeEventKeys, "View API keys in the event time range", (*App).eventKeys},
	{http.MethodGet, routeEvents, "List request events with pagination", func(a *App, req ManagementRequest) ManagementResponse {
		return a.listRequestEvents(req, viewAccess{})
	}},
	{http.MethodGet, routeErrors, "List error events with pagination", func(a *App, req ManagementRequest) ManagementResponse {
		return a.listRequestErrors(req, viewAccess{})
	}},
	{http.MethodGet, routeAnalysis, "View usage distribution", func(a *App, req ManagementRequest) ManagementResponse { return a.analysis(req, viewAccess{}) }},
	{http.MethodGet, routePluginLogs, "List plugin logs with pagination", (*App).listPluginLogs},
	{http.MethodDelete, routePluginLogs, "Clear plugin logs", func(a *App, _ ManagementRequest) ManagementResponse { return a.clearPluginLogs() }},
	{http.MethodGet, routeAuthFiles, "View auth files", func(a *App, _ ManagementRequest) ManagementResponse { return a.authFiles(viewAccess{}) }},
	{http.MethodGet, routeAuthQuota, "Query auth file quotas", func(a *App, req ManagementRequest) ManagementResponse { return a.authQuota(req, viewAccess{}) }},
	{http.MethodGet, routeIntegrations, "List host-managed provider integrations", (*App).listIntegrations},
	{http.MethodPost, routeIntegrations, "Save a host-managed provider integration", (*App).saveIntegration},
	{http.MethodDelete, routeIntegrations, "Retire a host-managed provider integration", (*App).deleteIntegration},
	{http.MethodPost, routeIntegrationsQuery, "Query provider models and subscription quotas", (*App).queryIntegration},
	{http.MethodPost, routeIntegrationsChannel, "Prepare the host channel for an integration", (*App).channelIntegration},
	{http.MethodPost, routeIntegrationsDevice, "Start Cline device sign-in", (*App).startIntegrationCline},
	{http.MethodPost, routeIntegrationsPoll, "Poll one Cline device sign-in", (*App).pollIntegrationCline},
	{http.MethodPost, routeIntegrationsRefresh, "Refresh a Cline OAuth credential", (*App).refreshIntegration},
	{http.MethodPost, routeIntegrationsCommit, "Commit a prepared credential replacement after publishing channels", (*App).commitIntegrationReplacement},
	{http.MethodPost, routeIntegrationsCancel, "Cancel pending Cline device sign-in or report its completion", (*App).cancelIntegrationCline},
}

type resourceEndpoint struct {
	path   string
	handle func(*App, ManagementRequest, viewAccess) ManagementResponse
}

var resourceEndpoints = []resourceEndpoint{
	{routeProfile, func(a *App, _ ManagementRequest, access viewAccess) ManagementResponse {
		return a.accountProfile(access)
	}},
	{routeSubscription, func(a *App, _ ManagementRequest, access viewAccess) ManagementResponse {
		return a.accountSubscription(access)
	}},
	{routeRouting, func(a *App, _ ManagementRequest, access viewAccess) ManagementResponse {
		return a.accountRouting(access)
	}},
	{routePrices, (*App).listPrices},
	{routeAnalysis, (*App).analysis},
	{routeEvents, (*App).listRequestEvents},
	{routeErrors, (*App).listRequestErrors},
	{routeAuthFiles, func(a *App, _ ManagementRequest, access viewAccess) ManagementResponse { return a.authFiles(access) }},
	{routeAuthQuota, (*App).authQuota},
}

func managementRegistration() ManagementRegistrationResponse {
	registration := ManagementRegistrationResponse{
		Routes:    make([]ManagementRoute, 0, len(managementEndpoints)),
		Resources: make([]ResourceRoute, 1, len(resourceEndpoints)+1),
	}
	registration.Resources[0] = ResourceRoute{Path: resourceBase + resourceUIPath, Menu: MenuLabel, Description: MenuDescription}
	for _, endpoint := range managementEndpoints {
		registration.Routes = append(registration.Routes, ManagementRoute{
			Method: endpoint.method, Path: managementBase + endpoint.path, Description: endpoint.description,
		})
	}
	for _, endpoint := range resourceEndpoints {
		registration.Resources = append(registration.Resources, ResourceRoute{Path: resourceBase + endpoint.path})
	}
	return registration
}

func (a *App) handleManagement(raw []byte) ([]byte, error) {
	var req ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("Parse management request: %w", errUnmarshal)
	}
	path := strings.TrimRight(req.Path, "/")
	if path == "" {
		path = req.Path
	}

	if req.Method == http.MethodGet && path == resourceBase+resourceUIPath {
		return OKEnvelope(ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":           []string{"text/html; charset=utf-8"},
				"Cache-Control":          []string{"private, no-store"},
				"Pragma":                 []string{"no-cache"},
				"Referrer-Policy":        []string{"no-referrer"},
				"X-Content-Type-Options": []string{"nosniff"},
				"Content-Security-Policy": []string{
					"default-src 'none'; script-src 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'unsafe-inline' https://cdn.jsdelivr.net; " +
						"font-src https://cdn.jsdelivr.net; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'",
				},
			},
			Body: uiHTML,
		})
	}
	if path != resourceBase && strings.HasPrefix(path, resourceBase+"/") {
		return OKEnvelope(a.routeResource(req, strings.TrimPrefix(path, resourceBase)))
	}
	if path != managementBase && !strings.HasPrefix(path, managementBase+"/") {
		return OKEnvelope(JSONError(http.StatusNotFound, "not_found", "Management route not found: "+req.Method+" "+req.Path))
	}
	return OKEnvelope(a.routeManagement(req, strings.TrimPrefix(path, managementBase)))
}

func (a *App) routeManagement(req ManagementRequest, suffix string) ManagementResponse {
	for _, endpoint := range managementEndpoints {
		if req.Method == endpoint.method && suffix == endpoint.path {
			if req.Query.Get("view") == "1" && req.Method != http.MethodGet {
				return a.mutateWithView(req, suffix, endpoint.handle)
			}
			return endpoint.handle(a, req)
		}
	}
	return JSONError(http.StatusNotFound, "not_found", "Management route not found: "+req.Method+" "+req.Path)
}

func errorResponse(err error) ManagementResponse {
	detail := messages.FromError(err)
	switch billing.KindOf(err) {
	case billing.KindInvalid:
		return jsonMessageError(http.StatusBadRequest, string(billing.KindInvalid), detail)
	case billing.KindNotFound:
		return jsonMessageError(http.StatusNotFound, string(billing.KindNotFound), detail)
	case billing.KindConflict:
		return jsonMessageError(http.StatusConflict, string(billing.KindConflict), detail)
	default:
		return jsonMessageError(http.StatusInternalServerError, "internal_error", detail)
	}
}
