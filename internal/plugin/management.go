package plugin

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"cpa-key-billing/internal/billing"
)

const (
	managementBase = "/v0/management/plugins/" + PluginID
	resourceBase   = "/v0/resource/plugins/" + PluginID
	resourceUIPath = "/ui"
)

//go:embed ui.html
var uiHTML []byte

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
	routeForwardedForBlock      = "/forwarded-for-block"
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
)

type managementEndpoint struct {
	method, path, description string
	handle                    func(*App, ManagementRequest) ManagementResponse
}

var managementEndpoints = []managementEndpoint{
	{http.MethodGet, routeKeys, "查看 API Key 状态", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"keys": a.keyRows()})
	}},
	{http.MethodGet, routeGroups, "查看 API Key 分组", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"groups": a.groupRows()})
	}},
	{http.MethodGet, routeAccessControl, "查看访问控制设置", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"access_control": a.store.AccessControl()})
	}},
	{http.MethodPut, routeAccessControl, "保存访问控制设置", (*App).setAccessControl},
	{http.MethodGet, routeForwardedForBlock, "查看 X-Forwarded-For 拦截设置", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"forwarded_for_block": a.store.ForwardedForBlock()})
	}},
	{http.MethodPut, routeForwardedForBlock, "保存 X-Forwarded-For 拦截设置", (*App).setForwardedForBlock},
	{http.MethodPost, routeGroups, "新建 API Key 分组", (*App).createGroup},
	{http.MethodPatch, routeGroups, "更新 API Key 分组", (*App).updateGroup},
	{http.MethodDelete, routeGroups, "删除 API Key 分组", (*App).deleteGroup},
	{http.MethodPut, routeKeysGroups, "替换 API Key 分组绑定", (*App).setKeyGroups},
	{http.MethodGet, routePlans, "查看订阅计划", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"plans": a.store.Plans()})
	}},
	{http.MethodGet, routeRoutes, "查看路由规则", func(a *App, _ ManagementRequest) ManagementResponse {
		return JSONResponse(http.StatusOK, map[string]any{"routes": a.routeRows()})
	}},
	{http.MethodGet, routeCredentials, "查看路由凭证选项", (*App).listCredentials},
	{http.MethodGet, routePrices, "查看模型定价", func(a *App, req ManagementRequest) ManagementResponse { return a.listPrices(req, viewAccess{}) }},
	{http.MethodGet, routeReferencePrices, "搜索模型参考价", (*App).searchReferencePrices},
	{http.MethodGet, routeReferencePricesStatus, "查看参考价状态", func(a *App, _ ManagementRequest) ManagementResponse { return a.referencePriceStatus() }},
	{http.MethodPost, routeReferencePricesRefresh, "更新参考价", func(a *App, _ ManagementRequest) ManagementResponse { return a.refreshReferencePrices() }},
	{http.MethodDelete, routePrices, "删除自定义模型价格", (*App).deletePrice},
	{http.MethodPut, routePrices, "更新模型定价", (*App).putPrices},
	{http.MethodPost, routePlans, "新建订阅计划", (*App).createPlan},
	{http.MethodPatch, routePlans, "更新订阅计划", (*App).updatePlan},
	{http.MethodDelete, routePlans, "删除订阅计划并解除 API Key 绑定", (*App).deletePlan},
	{http.MethodPost, routeRoutes, "新建路由规则", (*App).createRoute},
	{http.MethodPatch, routeRoutes, "更新路由规则", (*App).updateRoute},
	{http.MethodDelete, routeRoutes, "删除路由规则并解除相关绑定", (*App).deleteRoute},
	{http.MethodPut, routeKeysRoutes, "更新 API Key 路由绑定", (*App).setKeyRoutes},
	{http.MethodPost, routeKeysBind, "将 API Key 绑定到订阅计划", (*App).bindKey},
	{http.MethodPost, routeKeysUnbind, "解除 API Key 与订阅计划的绑定", (*App).unbindKey},
	{http.MethodPost, routeKeysReset, "重置指定 API Key 的订阅额度", (*App).resetKeys},
	{http.MethodPost, routeKeysLabel, "设置 API Key 备注", (*App).labelKey},
	{http.MethodPost, routeKeysConcurrency, "设置 API Key 最大并发请求数", (*App).setKeyConcurrency},
	{http.MethodPost, routeKeysSync, "同步 CLIProxyAPI 中的 API Key 列表", (*App).syncKeys},
	{http.MethodPost, routeCredentialsSync, "同步配置凭证", (*App).syncConfiguredCredentials},
	{http.MethodGet, routeEventKeys, "查看事件范围内的 API Key", (*App).eventKeys},
	{http.MethodGet, routeEvents, "分页查看请求事件", func(a *App, req ManagementRequest) ManagementResponse {
		return a.listRequestEvents(req, viewAccess{})
	}},
	{http.MethodGet, routeErrors, "分页查看错误事件", func(a *App, req ManagementRequest) ManagementResponse {
		return a.listRequestErrors(req, viewAccess{})
	}},
	{http.MethodGet, routeAnalysis, "查看用量分布", func(a *App, req ManagementRequest) ManagementResponse { return a.analysis(req, viewAccess{}) }},
	{http.MethodGet, routePluginLogs, "分页查看插件运行日志", (*App).listPluginLogs},
	{http.MethodDelete, routePluginLogs, "清空插件运行日志", func(a *App, _ ManagementRequest) ManagementResponse { return a.clearPluginLogs() }},
	{http.MethodGet, routeAuthFiles, "查看认证文件", func(a *App, _ ManagementRequest) ManagementResponse { return a.authFiles(viewAccess{}) }},
	{http.MethodGet, routeAuthQuota, "查询认证文件限额", func(a *App, req ManagementRequest) ManagementResponse { return a.authQuota(req, viewAccess{}) }},
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
		return nil, fmt.Errorf("解析管理请求：%w", errUnmarshal)
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
		return OKEnvelope(JSONError(http.StatusNotFound, "not_found", "管理路由不存在："+req.Method+" "+req.Path))
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
	return JSONError(http.StatusNotFound, "not_found", "管理路由不存在："+req.Method+" "+req.Path)
}

func errorResponse(err error) ManagementResponse {
	switch billing.KindOf(err) {
	case billing.KindInvalid:
		return JSONError(http.StatusBadRequest, string(billing.KindInvalid), err.Error())
	case billing.KindNotFound:
		return JSONError(http.StatusNotFound, string(billing.KindNotFound), err.Error())
	case billing.KindConflict:
		return JSONError(http.StatusConflict, string(billing.KindConflict), err.Error())
	default:
		return JSONError(http.StatusInternalServerError, "internal_error", err.Error())
	}
}
