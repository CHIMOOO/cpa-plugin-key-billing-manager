package plugin

import (
	"net/http"
	"slices"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestManagementGroupSwitchImmediatelyChangesAuthorization(t *testing.T) {
	app := newConfiguredApp(t)
	const key = "dummy-group-switch"
	callOK(t, app, http.MethodPost, routeKeysSync, nil, map[string]any{"keys": []string{key}}, http.StatusOK, nil)
	scope := billing.CallerScope(key)
	app.observeCandidates([]SchedulerAuthCandidate{{ID: "dummy-codex", Provider: "codex", Attributes: map[string]string{"source_backend": "file"}}})
	var response struct {
		Group groupRow `json:"group"`
	}
	callOK(t, app, http.MethodPost, routeGroups, nil, map[string]any{
		"name": "Switch", "disabled": true, "scopes": []string{scope},
		"rule": billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}}},
	}, http.StatusCreated, &response)
	if !response.Group.Disabled || !slices.Equal(response.Group.Scopes, []string{scope}) {
		t.Fatalf("disabled group create = %+v", response.Group)
	}
	if check := afterAuthForTest(t, app, scope, "dummy-codex"); !check.Terminate || check.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled group request = %+v", check)
	}
	id := response.Group.ID
	for _, disabled := range []bool{false, true, false} {
		callOK(t, app, http.MethodPatch, routeGroups, nil, map[string]any{"id": id, "disabled": disabled}, http.StatusOK, &response)
		if response.Group.Disabled != disabled || !slices.Equal(response.Group.Scopes, []string{scope}) || len(response.Group.Rule.CredentialProviders) != 1 {
			t.Fatalf("toggle changed group settings: %+v", response.Group)
		}
		if check := afterAuthForTest(t, app, scope, "dummy-codex"); check.Terminate != disabled {
			t.Fatalf("switch did not take effect immediately: %+v", check)
		}
	}
}
