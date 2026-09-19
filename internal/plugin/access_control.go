package plugin

import (
	"net/http"

	"cpa-key-billing/internal/billing"
)

func (a *App) setAccessControl(req ManagementRequest) ManagementResponse {
	var body struct {
		Enabled       *bool `json:"enabled"`
		DenyUngrouped *bool `json:"deny_ungrouped"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	if body.Enabled == nil || body.DenyUngrouped == nil {
		return JSONError(http.StatusBadRequest, "invalid", "enabled 和 deny_ungrouped 均为必填布尔值")
	}
	settings := billing.AccessControl{Enabled: *body.Enabled, DenyUngrouped: *body.DenyUngrouped}
	if err := a.store.SetAccessControl(settings); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"access_control": settings})
}

// A group's direct selection is validated like a route rule: only credential
// references it newly adds must still exist, so retired ones survive an edit.
func (a *App) createGroup(req ManagementRequest) ManagementResponse {
	var body struct {
		Name     string            `json:"name"`
		Disabled bool              `json:"disabled"`
		RouteIDs []string          `json:"route_ids"`
		Rule     billing.RouteRule `json:"rule"`
		Scopes   []string          `json:"scopes"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	rule, err := billing.NormalizeRouteRule(body.Rule)
	if err != nil {
		return errorResponse(err)
	}
	if response := a.validateNewCredentialRefs(rule.CredentialRefs(), nil); response != nil {
		return *response
	}
	group, err := a.store.CreateGroup(billing.KeyGroup{Name: body.Name, Disabled: body.Disabled, RouteIDs: body.RouteIDs, Rule: rule}, body.Scopes)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusCreated, map[string]any{"group": a.groupRow(group)})
}

func (a *App) updateGroup(req ManagementRequest) ManagementResponse {
	var body billing.GroupPatch
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	if body.Rule != nil {
		rule, err := billing.NormalizeRouteRule(*body.Rule)
		if err != nil {
			return errorResponse(err)
		}
		body.Rule = &rule
		var existing []string
		if group, ok := a.store.Group(body.ID); ok {
			existing = group.Rule.CredentialRefs()
		}
		if response := a.validateNewCredentialRefs(rule.CredentialRefs(), existing); response != nil {
			return *response
		}
	}
	group, err := a.store.UpdateGroup(body)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"group": a.groupRow(group)})
}

func (a *App) deleteGroup(req ManagementRequest) ManagementResponse {
	id := req.Query.Get("id")
	if err := a.store.DeleteGroup(id); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"deleted": id})
}

func (a *App) setKeyGroups(req ManagementRequest) ManagementResponse {
	var body struct {
		Scopes   []string `json:"scopes"`
		GroupIDs []string `json:"group_ids"`
	}
	if err := decodeStrict(req.Body, &body); err != nil {
		return errorResponse(err)
	}
	if body.GroupIDs == nil {
		return JSONError(http.StatusBadRequest, "invalid", "group_ids 必须是数组；显式传入 [] 才会清空分组")
	}
	if err := a.store.SetKeyGroups(body.Scopes, body.GroupIDs); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"updated": true})
}

func accessDeniedResponse(format, message string) RequestInterceptResponse {
	return RequestInterceptResponse{
		Terminate: true, StatusCode: http.StatusForbidden,
		ResponseHeaders: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		ResponseBody:    refusalBody(format, refusal{anthropicType: "permission_error", openaiType: "permission_error", openaiCode: "access_denied"}, message),
	}
}
