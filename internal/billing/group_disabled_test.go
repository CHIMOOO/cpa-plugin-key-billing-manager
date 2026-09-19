package billing

import (
	"slices"
	"testing"
)

func TestDisabledGroupsSuspendGrantsWithoutLosingMembership(t *testing.T) {
	store := newStore(t)
	const key = "dummy-group-switch"
	scope := CallerScope(key)
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	a, b := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b")
	group, err := store.CreateGroup(KeyGroup{Name: "Switch", Rule: RouteRule{CredentialIDs: []string{a}}}, []string{scope})
	if err != nil || group.Disabled {
		t.Fatalf("new group = %+v (%v)", group, err)
	}
	if !store.ResolveRouting(scope, "model", "model").AllowsCredential(a, CredentialSourceAuthFiles, "codex") {
		t.Fatal("default group did not grant its selected credential")
	}
	disabled := true
	updated, err := store.UpdateGroup(GroupPatch{ID: group.ID, Disabled: &disabled})
	if err != nil || !updated.Disabled || !slices.Equal(updated.Scopes, []string{scope}) || !slices.Equal(updated.Rule.CredentialIDs, []string{a}) {
		t.Fatalf("disabling changed membership or grants: %+v (%v)", updated, err)
	}
	if err := store.SetKeyRoutes(scope, RouteBindings{RouteRule: RouteRule{CredentialIDs: []string{a}}}); err != nil {
		t.Fatal(err)
	}
	decision := store.ResolveRouting(scope, "model", "model")
	if decision.AccessDenied == "" || decision.AllowsModel() || decision.AllowsCredential(a, CredentialSourceAuthFiles, "codex") {
		t.Fatalf("direct selection bypassed suspended group: %+v", decision)
	}
	if err := store.SetKeyRoutes(scope, RouteBindings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(KeyGroup{Name: "Other", Rule: RouteRule{CredentialIDs: []string{b}}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	decision = store.ResolveRouting(scope, "model", "model")
	if decision.AccessDenied != "" || decision.AllowsCredential(a, CredentialSourceAuthFiles, "codex") || !decision.AllowsCredential(b, CredentialSourceAuthFiles, "codex") {
		t.Fatalf("disabled grant participated in group union: %+v", decision)
	}
	disabled = false
	if _, err := store.UpdateGroup(GroupPatch{ID: group.ID, Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	decision = store.ResolveRouting(scope, "model", "model")
	if !decision.AllowsCredential(a, CredentialSourceAuthFiles, "codex") || !decision.AllowsCredential(b, CredentialSourceAuthFiles, "codex") {
		t.Fatalf("reenabling did not restore original grants: %+v", decision)
	}
}

func TestDisabledGroupsDoNotContributeDenialsOrBrokenRoutes(t *testing.T) {
	ref := CredentialFingerprint("dummy-allowed")
	state := NewState()
	state.Groups = []KeyGroup{
		{ID: "on", Name: "On", Rule: RouteRule{CredentialIDs: []string{ref}}},
		{ID: "off", Name: "Off", Disabled: true, RouteIDs: []string{"missing"}, Rule: RouteRule{DeniedCredentialIDs: []string{ref}, DeniedModels: []string{"model"}}},
	}
	decision := resolveRoutingState(state, &KeyState{GroupIDs: []string{"off", "on"}})
	decision.Model = "model"
	if decision.ConfigurationError != "" || !decision.AllowsModel() || !decision.AllowsCredential(ref, CredentialSourceAuthFiles, "codex") {
		t.Fatalf("disabled group still participates: %+v", decision)
	}
}
