package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestGroupManagementAndPolicyPersistence(t *testing.T) {
	app := newConfiguredApp(t)
	const dummyKey = "dummy-group-key"
	if _, err := app.store.SyncKeys([]string{dummyKey}, false); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope(dummyKey)
	if _, err := app.store.CreateRoute(billing.Route{ID: "selected", Name: "Selected", Rule: billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-selected")}}}, nil); err != nil {
		t.Fatal(err)
	}
	var initial struct {
		AccessControl billing.AccessControl `json:"access_control"`
	}
	callOK(t, app, http.MethodGet, routeAccessControl, nil, nil, 200, &initial)
	if !initial.AccessControl.Enabled || initial.AccessControl.DenyUngrouped {
		t.Fatalf("defaults: %+v", initial)
	}
	var result struct {
		Group billing.GroupView `json:"group"`
		View  struct {
			Groups        []billing.GroupView   `json:"groups"`
			Keys          []billing.KeyView     `json:"keys"`
			AccessControl billing.AccessControl `json:"access_control"`
		} `json:"view"`
	}
	callOK(t, app, http.MethodPost, routeGroups, url.Values{"view": {"1"}}, map[string]any{"data": map[string]any{"name": "Team", "route_ids": []string{"selected"}, "scopes": []string{scope}}}, 201, &result)
	if len(result.View.Groups) != 1 || !slices.Equal(result.View.Keys[0].GroupIDs, []string{result.Group.ID}) {
		t.Fatalf("missing mutation view: %+v", result)
	}
	callOK(t, app, http.MethodPut, routeAccessControl, nil, map[string]bool{"enabled": true, "deny_ungrouped": true}, 200, nil)
	for _, body := range []map[string]any{{"scopes": []string{scope}}, {"scopes": []string{scope}, "group_ids": nil}} {
		if response := callManagement(t, app, http.MethodPut, routeKeysGroups, nil, body); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("implicit clearing accepted: %d", response.StatusCode)
		}
	}
	if key, _ := app.store.KeyViewForScope(scope); !slices.Equal(key.GroupIDs, []string{result.Group.ID}) {
		t.Fatal("invalid group replacement changed memberships")
	}
	callOK(t, app, http.MethodPut, routeKeysGroups, nil, map[string]any{"scopes": []string{scope}, "group_ids": []string{}}, 200, nil)
	if d := app.store.ResolveRouting(scope, "model", "model"); d.AccessDenied == "" {
		t.Fatal("ungrouped key was not blocked")
	}
	callOK(t, app, http.MethodPut, routeKeysGroups, nil, map[string]any{"scopes": []string{scope}, "group_ids": []string{result.Group.ID}}, 200, nil)
	if response := callManagement(t, app, http.MethodDelete, routeRoutes, url.Values{"id": {"selected"}}, nil); response.StatusCode != 409 {
		t.Fatalf("bound route deletion status: %d", response.StatusCode)
	}
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": result.Group.ID, "name": "Renamed"}, 200, nil)
	callOK(t, app, http.MethodDelete, routeGroups, url.Values{"id": {result.Group.ID}}, nil, 200, nil)
	if groups := app.store.GroupViews(); len(groups) != 0 {
		t.Fatal(groups)
	}
	if keys := readKeys(t, app); len(keys[0].GroupIDs) != 0 {
		t.Fatal(keys)
	}
	if response := callManagement(t, app, http.MethodPut, routeAccessControl, nil, `{"enabled":false}`); response.StatusCode != 400 {
		t.Fatalf("partial PUT accepted: %d", response.StatusCode)
	}
}

func TestAccessControlRejectsUngroupedAdmissionAndScheduler(t *testing.T) {
	app := newConfiguredApp(t)
	if err := app.store.SetAccessControl(billing.AccessControl{Enabled: true, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "unknown"} {
		raw, err := app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{Model: "model", Metadata: map[string]any{MetadataCallerScope: scope}}))
		if err != nil {
			t.Fatal(err)
		}
		var admission RequestInterceptResponse
		decodeResult(t, raw, &admission)
		if !admission.Terminate || admission.StatusCode != 403 {
			t.Fatalf("admission: %+v", admission)
		}
		raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, SchedulerAuthCandidate{ID: "dummy-auth", Provider: "codex"})))
		if err != nil {
			t.Fatal(err)
		}
		var envelope Envelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != 403 {
			t.Fatalf("scheduler: %+v", envelope)
		}
	}
}

func TestGroupCredentialSelectionRejectsUnselectedAndDisabledBypasses(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-selected")}})
	if err := app.store.SetKeyRoutes(scope, billing.RouteBindings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreateGroup(billing.KeyGroup{Name: "Team", RouteIDs: []string{"route-test"}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	req := schedulerRequest(scope, SchedulerAuthCandidate{ID: "dummy-selected", Provider: "codex"}, SchedulerAuthCandidate{ID: "unselected", Provider: "codex"})
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	decodeResult(t, raw, &response)
	if !response.Handled || response.AuthID != "dummy-selected" {
		t.Fatalf("group credential selection: %+v", response)
	}
	if err := app.store.SetAccessControl(billing.AccessControl{}); err != nil {
		t.Fatal(err)
	}
	raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &response)
	if response.Handled {
		t.Fatalf("disabled access control handled scheduler: %+v", response)
	}
}

func TestDisablingAccessControlPreservesQuotaEnforcement(t *testing.T) {
	app := exhaustedApp(t, time.Hour)
	if err := app.store.SetAccessControl(billing.AccessControl{}); err != nil {
		t.Fatal(err)
	}
	if response := callIntercept(t, app, "openai"); !response.Terminate || response.StatusCode != 429 {
		t.Fatalf("quota bypassed: %+v", response)
	}
}
