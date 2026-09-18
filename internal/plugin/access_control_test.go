package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
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

func TestGroupDirectRuleThroughManagementAPI(t *testing.T) {
	app := newConfiguredApp(t)
	const key = "sk-group-direct-rule-0001"
	callOK(t, app, http.MethodPost, routeKeysSync, nil, map[string]any{"keys": []string{key}}, http.StatusOK, nil)
	scope := billing.CallerScope(key)
	allowedFile := `{"id":"dummy-group-allowed","provider":"codex","source":"file","path":"/auth/allowed.json","name":"allowed.json","email":"allowed@example.test"}`
	otherFile := `{"id":"dummy-group-other","provider":"codex","source":"file","path":"/auth/other.json","name":"other.json","email":"other@example.test"}`
	hostFiles, hostDown := `{"files":[`+allowedFile+`,`+otherFile+`]}`, false
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("host method=%q", method)
		}
		if hostDown {
			return nil, errors.New("dummy host failure")
		}
		return json.RawMessage(hostFiles), nil
	})
	allowed, other := billing.CredentialFingerprint("dummy-group-allowed"), billing.CredentialFingerprint("dummy-group-other")
	missing := billing.CredentialFingerprint("dummy-group-missing")

	response := callManagement(t, app, http.MethodPost, routeGroups, nil, map[string]any{
		"name": "Missing", "rule": map[string]any{"credential_ids": []string{missing}}, "scopes": []string{scope},
	})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "上游凭证已不存在") || len(app.store.GroupViews()) != 0 {
		t.Fatalf("missing credential accepted: %d %s", response.StatusCode, response.Body)
	}

	// No route at all: the group grants exactly the credential it selects.
	response = callManagement(t, app, http.MethodPost, routeGroups, url.Values{"view": {"1"}}, map[string]any{"data": map[string]any{
		"name": "Direct", "route_ids": []string{}, "rule": map[string]any{"credential_ids": []string{allowed}}, "scopes": []string{scope},
	}})
	if response.StatusCode != http.StatusCreated || strings.Contains(string(response.Body), "dummy-group-allowed") {
		t.Fatalf("create status=%d body=%s", response.StatusCode, response.Body)
	}
	var created struct {
		Group map[string]json.RawMessage `json:"group"`
		View  struct {
			Groups []groupRow `json:"groups"`
		} `json:"view"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	var rule map[string]json.RawMessage
	if err := json.Unmarshal(created.Group["rule"], &rule); err != nil || len(rule) != 6 {
		t.Fatalf("group rule shape = %s (%v)", created.Group["rule"], err)
	}
	for field, value := range rule {
		if !strings.HasPrefix(string(value), "[") {
			t.Fatalf("rule.%s = %s, want an array", field, value)
		}
	}
	const label = "codex · allowed@example.test"
	var labels map[string]string
	if err := json.Unmarshal(created.Group["credential_labels"], &labels); err != nil || labels[allowed] != label {
		t.Fatalf("created credential labels = %s (%v)", created.Group["credential_labels"], err)
	}
	if len(created.View.Groups) != 1 || created.View.Groups[0].CredentialLabels[allowed] != label ||
		!slices.Equal(created.View.Groups[0].Rule.CredentialIDs, []string{allowed}) || !slices.Equal(created.View.Groups[0].Scopes, []string{scope}) {
		t.Fatalf("mutation view groups = %+v", created.View.Groups)
	}
	id := created.View.Groups[0].ID
	var listed struct {
		Groups []groupRow `json:"groups"`
	}
	callOK(t, app, http.MethodGet, routeGroups, nil, nil, http.StatusOK, &listed)
	if len(listed.Groups) != 1 || listed.Groups[0].CredentialLabels[allowed] != label || len(listed.Groups[0].RouteIDs) != 0 ||
		!slices.Equal(listed.Groups[0].Rule.CredentialIDs, []string{allowed}) || listed.Groups[0].Rule.DeniedModels == nil {
		t.Fatalf("listed groups = %+v", listed.Groups)
	}
	// Mutation views of the other group-related endpoints use the same rows.
	var viewed struct {
		View struct {
			Groups []groupRow `json:"groups"`
		} `json:"view"`
	}
	callOK(t, app, http.MethodPut, routeKeysGroups, url.Values{"view": {"1"}}, map[string]any{"data": map[string]any{"scopes": []string{scope}, "group_ids": []string{id}}}, http.StatusOK, &viewed)
	if len(viewed.View.Groups) != 1 || viewed.View.Groups[0].CredentialLabels[allowed] != label {
		t.Fatalf("key-group mutation view = %+v", viewed.View.Groups)
	}

	// Scheduler pick, the after-auth check and admission all honour it.
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope,
		SchedulerAuthCandidate{ID: "dummy-group-other", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-group-allowed", Provider: "codex"})))
	if err != nil {
		t.Fatal(err)
	}
	var picked SchedulerPickResponse
	decodeResult(t, raw, &picked)
	if !picked.Handled || picked.AuthID != "dummy-group-allowed" {
		t.Fatalf("scheduler pick = %+v", picked)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-group-allowed"); response.Terminate {
		t.Fatalf("selected credential refused: %+v", response)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-group-other"); !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("unselected credential accepted: %+v", response)
	}
	var routing accountRoutingResponse
	if err := json.Unmarshal(callAccount(t, app, routeRouting, key, nil).Body, &routing); err != nil {
		t.Fatal(err)
	}
	if !routing.RoutingValid || !routing.CredentialsRestricted || len(routing.Credentials) != 1 || routing.Credentials[0].Name != "allowed@example.test" {
		t.Fatalf("self-service routing = %+v", routing)
	}

	// Only newly added references are checked: an unchanged one needs no host.
	hostDown = true
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{"credential_ids": []string{allowed}, "denied_models": []string{"blocked"}}}, http.StatusOK, nil)
	if response := callManagement(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{"credential_ids": []string{allowed, other}}}); response.StatusCode != http.StatusBadGateway {
		t.Fatalf("host failure status=%d body=%s", response.StatusCode, response.Body)
	}
	// A retired reference survives while another one is added.
	hostDown, hostFiles = false, `{"files":[`+otherFile+`]}`
	var patched struct {
		Group groupRow `json:"group"`
	}
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{"credential_ids": []string{allowed, other}}}, http.StatusOK, &patched)
	wantIDs := []string{allowed, other}
	if !slices.Equal(patched.Group.Rule.CredentialIDs, wantIDs) || len(patched.Group.Rule.DeniedModels) != 0 ||
		patched.Group.CredentialLabels[other] != "codex · other@example.test" {
		t.Fatalf("patched group = %+v", patched.Group)
	}
	if response := callManagement(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{"credential_ids": []string{allowed, missing}}}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("new missing reference status=%d body=%s", response.StatusCode, response.Body)
	}
	// Omitting the rule keeps it.
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "name": "Renamed"}, http.StatusOK, &patched)
	if patched.Group.Name != "Renamed" || !slices.Equal(patched.Group.Rule.CredentialIDs, wantIDs) {
		t.Fatalf("rename changed the rule: %+v", patched.Group)
	}
	if response := callManagement(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{"models": []string{"m"}, "denied_models": []string{"m"}}}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflicting rule status=%d body=%s", response.StatusCode, response.Body)
	}

	// Deny references are checked and labelled like allow references.
	deniedFile := `{"id":"dummy-group-denied","provider":"codex","source":"file","path":"/auth/denied.json","name":"denied.json","email":"denied@example.test"}`
	hostFiles = `{"files":[` + otherFile + `,` + deniedFile + `]}`
	denied := billing.CredentialFingerprint("dummy-group-denied")
	if response := callManagement(t, app, http.MethodPost, routeGroups, nil, map[string]any{
		"name": "Missing deny", "rule": map[string]any{"credential_ids": []string{other}, "denied_credential_ids": []string{missing}},
	}); response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "上游凭证已不存在") || len(app.store.GroupViews()) != 1 {
		t.Fatalf("missing denied credential accepted on create: %d %s", response.StatusCode, response.Body)
	}
	if response := callManagement(t, app, http.MethodPatch, routeGroups, nil, map[string]any{
		"id": id, "rule": map[string]any{"credential_ids": wantIDs, "denied_credential_ids": []string{missing}},
	}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing denied credential accepted on update: %d %s", response.StatusCode, response.Body)
	}
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{
		"id": id, "rule": map[string]any{"credential_ids": wantIDs, "denied_credential_ids": []string{denied}},
	}, http.StatusOK, &patched)
	const deniedLabel = "codex · denied@example.test"
	if !slices.Equal(patched.Group.Rule.DeniedCredentialIDs, []string{denied}) || patched.Group.CredentialLabels[denied] != deniedLabel {
		t.Fatalf("denied credential not saved or labelled: %+v", patched.Group)
	}
	callOK(t, app, http.MethodGet, routeGroups, nil, nil, http.StatusOK, &listed)
	if len(listed.Groups) != 1 || listed.Groups[0].CredentialLabels[denied] != deniedLabel {
		t.Fatalf("listed denied label = %+v", listed.Groups)
	}

	// Clearing the selection leaves an unconfigured group, which denies access.
	callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "rule": map[string]any{}}, http.StatusOK, nil)
	raw, err = app.HandleMethod(MethodRequestInterceptBefore, mustMarshal(t, RequestInterceptRequest{
		SourceFormat: "openai", Model: "gpt-5.6", Metadata: map[string]any{MetadataCallerScope: scope},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var admission RequestInterceptResponse
	decodeResult(t, raw, &admission)
	if !admission.Terminate || admission.StatusCode != http.StatusForbidden || !strings.Contains(string(admission.ResponseBody), "尚未绑定路由规则或上游凭证") {
		t.Fatalf("unconfigured group admission = %+v (%s)", admission, admission.ResponseBody)
	}
}

func TestGroupDirectCredentialsIgnoredWhenAccessControlDisabled(t *testing.T) {
	app := newConfiguredApp(t)
	const key = "sk-group-direct-off-0001"
	if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope(key)
	if _, err := app.store.CreateGroup(billing.KeyGroup{Name: "Direct", Rule: billing.RouteRule{
		CredentialIDs: []string{billing.CredentialFingerprint("dummy-selected")},
	}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	req := schedulerRequest(scope, SchedulerAuthCandidate{ID: "dummy-selected", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-unselected", Provider: "codex"})
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	decodeResult(t, raw, &response)
	if !response.Handled || response.AuthID != "dummy-selected" {
		t.Fatalf("group direct selection: %+v", response)
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
	if after := afterAuthForTest(t, app, scope, "dummy-unselected"); after.Terminate {
		t.Fatalf("disabled access control rejected: %+v", after)
	}
}
