package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestGroupRoutingModesManagementAndBusinessUseSamePool(t *testing.T) {
	app := newConfiguredApp(t)
	const key = "dummy-exclusive-ui"
	scope := billing.CallerScope(key)
	if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("unexpected host method %s", method)
		}
		return json.RawMessage(`{"files":[{"id":"dummy-a","provider":"codex","source":"file","path":"/auth/a.json","email":"a@example.test"},{"id":"dummy-b","provider":"codex","source":"file","path":"/auth/b.json","email":"b@example.test"},{"id":"dummy-c","provider":"codex","source":"file","path":"/auth/c.json","email":"c@example.test"}]}`), nil
	})
	a, b, c := billing.CredentialFingerprint("dummy-a"), billing.CredentialFingerprint("dummy-b"), billing.CredentialFingerprint("dummy-c")
	var ordinary, exclusive, common struct {
		Group groupRow `json:"group"`
	}
	callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Ordinary", "scopes": []string{scope}, "rule": billing.RouteRule{CredentialIDs: []string{a}}}, http.StatusCreated, &ordinary)
	callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Exclusive", "routing_mode": "exclusive", "scopes": []string{scope}, "rule": billing.RouteRule{CredentialIDs: []string{b}}}, http.StatusCreated, &exclusive)
	callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Common", "routing_mode": "common", "scopes": []string{scope}, "rule": billing.RouteRule{CredentialIDs: []string{c}}}, http.StatusCreated, &common)
	if ordinary.Group.RoutingMode != "ordinary" || exclusive.Group.RoutingMode != "exclusive" || common.Group.RoutingMode != "common" {
		t.Fatal("groupRow dropped routing mode")
	}
	var listed struct {
		Groups []groupRow `json:"groups"`
	}
	callOK(t, app, http.MethodGet, routeGroups, nil, nil, http.StatusOK, &listed)
	if len(listed.Groups) != 3 || listed.Groups[1].RoutingMode != "exclusive" {
		t.Fatalf("GET lost modes: %+v", listed)
	}
	if response := callManagement(t, app, http.MethodPost, routeGroups, nil, map[string]any{"name": "Conflict", "routing_mode": "exclusive", "scopes": []string{scope}}); response.StatusCode != http.StatusConflict || !strings.Contains(string(response.Body), "backend.groups.exclusive_conflict") {
		t.Fatalf("conflict validation = %d %s", response.StatusCode, response.Body)
	}
	if response := callManagement(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": exclusive.Group.ID, "routing_mode": "unknown"}); response.StatusCode != http.StatusBadRequest || !strings.Contains(string(response.Body), "backend.groups.invalid_routing_mode") {
		t.Fatalf("mode validation = %d %s", response.StatusCode, response.Body)
	}
	// Single-key direct selection cannot re-add the ordinary group's account.
	if err := app.store.SetKeyRoutes(scope, billing.RouteBindings{RouteRule: billing.RouteRule{CredentialIDs: []string{a, b}}}); err != nil {
		t.Fatal(err)
	}
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, SchedulerAuthCandidate{ID: "dummy-a", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-b", Provider: "codex"}, SchedulerAuthCandidate{ID: "dummy-c", Provider: "codex"})))
	if err != nil {
		t.Fatal(err)
	}
	var picked SchedulerPickResponse
	decodeResult(t, raw, &picked)
	if !picked.Handled || picked.AuthID != "dummy-b" {
		t.Fatalf("scheduler escaped intersection: %+v", picked)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-a"); !response.Terminate {
		t.Fatal("direct key allow escaped exclusive group")
	}
	if response := afterAuthForTest(t, app, scope, "dummy-b"); response.Terminate {
		t.Fatalf("intersection denied: %+v", response)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-c"); !response.Terminate {
		t.Fatal("common pool escaped direct constraint")
	}
	var routing accountRoutingResponse
	if err := json.Unmarshal(callAccount(t, app, routeRouting, key, nil).Body, &routing); err != nil {
		t.Fatal(err)
	}
	if !routing.RoutingValid || len(routing.Credentials) != 2 {
		t.Fatalf("account pool = %+v", routing)
	}
	for _, item := range routing.Credentials {
		if item.Name == "a@example.test" || item.Name == "b@example.test" && item.Denied || item.Name == "c@example.test" && !item.Denied {
			t.Fatalf("account pool contradicts admission: %+v", routing)
		}
	}
	decision := app.store.ResolveRouting(scope, "", "")
	withoutConstraint := decision
	withoutConstraint.CredentialConstraint = nil
	if routingPoolKey("model", decision) == routingPoolKey("model", withoutConstraint) {
		t.Fatal("scheduler merged distinct constrained pools")
	}
}
