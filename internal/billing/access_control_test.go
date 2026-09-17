package billing

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestAccessControlDefaultsAndUngroupedKeys(t *testing.T) {
	store := newStore(t)
	if got := store.AccessControl(); !got.Enabled || got.DenyUngrouped {
		t.Fatalf("defaults = %+v", got)
	}
	if decision := store.ResolveRouting("unknown", "model", "model"); !decision.AllowsModel() || !decision.AllowsCredential("ref", "auth-files", "codex") {
		t.Fatalf("default ungrouped policy = %+v", decision)
	}
	if err := store.SetAccessControl(AccessControl{Enabled: true, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SyncKeys([]string{"dummy-new-key"}, false); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"", "unknown", CallerScope("dummy-new-key")} {
		d := store.ResolveRouting(scope, "model", "model")
		if d.AccessDenied == "" || d.AllowsModel() || d.AllowsCredential("ref", "auth-files", "codex") {
			t.Fatalf("ungrouped scope %q escaped: %+v", scope, d)
		}
	}
	if err := store.SetAccessControl(AccessControl{Enabled: false, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting("unknown", "model", "model")
	if !d.AllowsModel() || d.RestrictsCredentials() || d.AccessDenied != "" {
		t.Fatalf("disabled policy still enforced: %+v", d)
	}
}

func TestGroupsUnionAllowlistAndDenyPrecedence(t *testing.T) {
	store := newStore(t)
	scope := CallerScope("dummy-grouped")
	if _, err := store.SyncKeys([]string{"dummy-grouped"}, false); err != nil {
		t.Fatal(err)
	}
	a, b, c := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b"), CredentialFingerprint("dummy-c")
	for _, route := range []Route{
		{ID: "a", Name: "A", Rule: RouteRule{Models: []string{"model-a"}, CredentialIDs: []string{a}}},
		{ID: "b", Name: "B", Rule: RouteRule{Models: []string{"model-b"}, CredentialIDs: []string{b}, DeniedCredentialIDs: []string{a}}},
	} {
		if _, err := store.CreateRoute(route, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if _, err := store.CreateGroup(KeyGroup{ID: "group-" + id, Name: id, RouteIDs: []string{id}}, []string{scope}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetKeyRoutes(scope, RouteBindings{RouteRule: RouteRule{Models: []string{"direct"}, CredentialIDs: []string{c}}}); err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting(scope, "model-a", "model-a")
	if !d.AllowsModel() || d.AllowsCredential(a, "auth-files", "codex") || !d.AllowsCredential(b, "auth-files", "codex") || !d.AllowsCredential(c, "auth-files", "codex") || d.AllowsCredential("unknown", "auth-files", "codex") {
		t.Fatalf("group/direct union or deny precedence incorrect: %+v", d)
	}
	if !slices.Equal(d.Models, []string{"direct", "model-a", "model-b"}) {
		t.Fatal(d.Models)
	}
	for _, route := range store.RouteViews() {
		if route.BoundKeyCount != 1 || route.FullyUnrestrictedKeys != 0 {
			t.Fatalf("group memberships missing from route counts: %+v", route)
		}
	}
	if _, err := store.DeleteRoute("a"); KindOf(err) != KindConflict {
		t.Fatalf("bound group route deletion: %v", err)
	}
	if err := store.SetKeyGroups([]string{scope}, []string{"group-a", "group-a"}); err != nil {
		t.Fatal(err)
	}
	view, _ := store.KeyViewForScope(scope)
	if !slices.Equal(view.GroupIDs, []string{"group-a"}) {
		t.Fatalf("group replacement = %v", view.GroupIDs)
	}
	if err := store.SetKeyGroups([]string{scope, "missing"}, []string{"group-b"}); KindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	view, _ = store.KeyViewForScope(scope)
	if !slices.Equal(view.GroupIDs, []string{"group-a"}) {
		t.Fatal("partial bulk assignment committed")
	}
}

func TestGroupsAndManagedCredentialsFailClosed(t *testing.T) {
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Groups = []KeyGroup{{ID: "empty", Name: "Empty"}, {ID: "broken", Name: "Broken", RouteIDs: []string{"missing"}}}
		state.Routes = []Route{{ID: "models", Name: "Models", Rule: RouteRule{Models: []string{"model"}}}}
		state.Keys["empty"] = &KeyState{GroupIDs: []string{"empty"}}
		state.Keys["broken"] = &KeyState{GroupIDs: []string{"broken"}}
		state.Keys["missing"] = &KeyState{GroupIDs: []string{"missing"}}
		state.Keys["models"] = &KeyState{RouteBindings: RouteBindings{RouteIDs: []string{"models"}}}
		state.Keys["deny"] = &KeyState{RouteBindings: RouteBindings{RouteRule: RouteRule{DeniedModels: []string{"blocked"}}}}
	})
	for _, scope := range []string{"empty", "broken", "missing", "models", "deny"} {
		d := store.ResolveRouting(scope, "model", "model")
		if d.AllowsCredential("unselected", "auth-files", "codex") {
			t.Fatalf("%s accepted an unselected credential: %+v", scope, d)
		}
		if scope == "empty" && d.AccessDenied == "" {
			t.Fatal("empty group was treated as authorized")
		}
		if (scope == "broken" || scope == "missing") && d.ConfigurationError == "" {
			t.Fatal("missing configuration accepted")
		}
	}
	if err := store.SetAccessControl(AccessControl{}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"empty", "broken", "missing", "models", "deny"} {
		if d := store.ResolveRouting(scope, "blocked", "blocked"); !d.AllowsModel() || !d.AllowsCredential("unselected", "auth-files", "codex") {
			t.Fatalf("disabled scope %q: %+v", scope, d)
		}
	}
}

func TestAccessControlManagementRollsBackOnPersistenceFailure(t *testing.T) {
	store, repo := newStoreWithRepository(t)
	store.ReplaceAll(func(state *State) { state.Keys["scope"] = &KeyState{} })
	group, err := store.CreateGroup(KeyGroup{Name: "Original"}, []string{"scope"})
	if err != nil {
		t.Fatal(err)
	}
	before := store.GroupViews()
	repo.fail = errors.New("disk full")
	name, scopes := "Changed", []string{}
	if _, err := store.UpdateGroup(GroupPatch{ID: group.ID, Name: &name, Scopes: &scopes}); err == nil {
		t.Fatal("write failure not returned")
	}
	if err := store.DeleteGroup(group.ID); err == nil {
		t.Fatal("delete write failure not returned")
	}
	if err := store.SetAccessControl(AccessControl{}); err == nil {
		t.Fatal("policy write failure not returned")
	}
	if !reflect.DeepEqual(before, store.GroupViews()) || !store.AccessControl().Enabled {
		t.Fatal("failed writes changed active policy")
	}
	view, _ := store.KeyViewForScope("scope")
	if !slices.Equal(view.GroupIDs, []string{group.ID}) {
		t.Fatal("failed write changed memberships")
	}
}

func TestClearingDirectCredentialSelectionStaysRestricted(t *testing.T) {
	store := newStore(t)
	const key = "dummy-clear-selection"
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := CallerScope(key)
	ref := CredentialFingerprint("dummy-auth")
	if err := store.SetKeyRoutes(scope, RouteBindings{RouteRule: RouteRule{CredentialIDs: []string{ref}}}); err != nil {
		t.Fatal(err)
	}
	if !store.ResolveRouting(scope, "model", "model").AllowsCredential(ref, "auth-files", "codex") {
		t.Fatal("selected credential rejected")
	}
	if err := store.SetKeyRoutes(scope, RouteBindings{}); err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting(scope, "model", "model")
	if !d.RestrictsCredentials() || d.AllowsCredential(ref, "auth-files", "codex") {
		t.Fatalf("clearing the last selection reopened access: %+v", d)
	}
	view, _ := store.KeyViewForScope(scope)
	if !view.RouteBindings.Configured {
		t.Fatal("explicit configuration marker was lost")
	}
}
