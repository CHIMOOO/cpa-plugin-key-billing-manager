package billing

import (
	"errors"
	"reflect"
	"slices"
	"strings"
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

func TestGroupDirectSelectionGrantsExactlyItsCredentials(t *testing.T) {
	store := newStore(t)
	const key = "dummy-direct-group"
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := CallerScope(key)
	selected, other := CredentialFingerprint("dummy-selected"), CredentialFingerprint("dummy-other")
	group, err := store.CreateGroup(KeyGroup{Name: "Direct", Rule: RouteRule{CredentialIDs: []string{selected}}}, []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(group.RouteIDs) != 0 || !slices.Equal(group.Rule.CredentialIDs, []string{selected}) {
		t.Fatalf("created group = %+v", group)
	}
	d := store.ResolveRouting(scope, "any-model", "any-model")
	if d.AccessDenied != "" || d.ConfigurationError != "" || !d.AllowsModel() || d.RestrictsModels() {
		t.Fatalf("direct credential group restricted models or denied access: %+v", d)
	}
	if !d.RequireCredentialAllowlist || !d.AllowsCredential(selected, "auth-files", "codex") ||
		d.AllowsCredential(other, "auth-files", "codex") || d.AllowsCredential("", "auth-files", "codex") {
		t.Fatalf("direct credential group granted the wrong credentials: %+v", d)
	}
	if !slices.Equal(d.CredentialIDs, []string{selected}) {
		t.Fatalf("effective allowlist = %v", d.CredentialIDs)
	}

	// Only models, no credentials: the group is configured, yet grants no upstream.
	models := RouteRule{Models: []string{"only-model"}}
	if _, err := store.UpdateGroup(GroupPatch{ID: group.ID, Rule: &models}); err != nil {
		t.Fatal(err)
	}
	d = store.ResolveRouting(scope, "only-model", "only-model")
	if d.AccessDenied != "" || !d.AllowsModel() || d.AllowsCredential(selected, "auth-files", "codex") {
		t.Fatalf("model-only group: %+v", d)
	}
	if d = store.ResolveRouting(scope, "other-model", "other-model"); d.AllowsModel() {
		t.Fatalf("model-only group allowed another model: %+v", d)
	}
}

func TestGroupDirectDenyWinsOverRouteAllow(t *testing.T) {
	store := newStore(t)
	const key = "dummy-deny-group"
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := CallerScope(key)
	a, b := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b")
	if _, err := store.CreateRoute(Route{ID: "allow", Name: "Allow", Rule: RouteRule{Models: []string{"model-a", "model-b"}, CredentialIDs: []string{a, b}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(KeyGroup{ID: "routes", Name: "Routes", RouteIDs: []string{"allow"}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(KeyGroup{ID: "deny", Name: "Deny", Rule: RouteRule{
		DeniedModels: []string{"MODEL-B"}, DeniedCredentialIDs: []string{a},
		DeniedCredentialProviders: []CredentialProviderSelector{{Source: CredentialSourceAIProviders, Provider: "claude"}},
	}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting(scope, "model-a", "model-a")
	if !d.AllowsModel() || d.AllowsCredential(a, "auth-files", "codex") || !d.AllowsCredential(b, "auth-files", "codex") ||
		d.AllowsCredential(b, "ai-providers", "claude") {
		t.Fatalf("group deny did not take precedence: %+v", d)
	}
	if d = store.ResolveRouting(scope, "model-b", "model-b"); d.AllowsModel() {
		t.Fatalf("group model deny bypassed by route allow: %+v", d)
	}
	// The key's own allowlist cannot override a group deny either.
	if err := store.SetKeyRoutes(scope, RouteBindings{RouteRule: RouteRule{CredentialIDs: []string{a}}}); err != nil {
		t.Fatal(err)
	}
	if d = store.ResolveRouting(scope, "model-a", "model-a"); d.AllowsCredential(a, "auth-files", "codex") {
		t.Fatalf("direct key allow bypassed group deny: %+v", d)
	}
}

func TestGroupMergesItsRoutesWithItsOwnRule(t *testing.T) {
	store := newStore(t)
	const key = "dummy-route-and-rule-group"
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := CallerScope(key)
	a, b := CredentialFingerprint("dummy-route-a"), CredentialFingerprint("dummy-rule-b")
	if _, err := store.CreateRoute(Route{ID: "r", Name: "R", Rule: RouteRule{CredentialIDs: []string{a}}}, nil); err != nil {
		t.Fatal(err)
	}
	group, err := store.CreateGroup(KeyGroup{Name: "Both", RouteIDs: []string{"r"}, Rule: RouteRule{CredentialIDs: []string{b}, DeniedModels: []string{"m"}}}, []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting(scope, "model", "model")
	if d.AccessDenied != "" || !d.AllowsModel() || !d.AllowsCredential(a, "auth-files", "codex") || !d.AllowsCredential(b, "auth-files", "codex") ||
		d.AllowsCredential(CredentialFingerprint("dummy-other"), "auth-files", "codex") {
		t.Fatalf("one group's route and rule were not merged: %+v", d)
	}
	if d = store.ResolveRouting(scope, "m", "m"); d.AllowsModel() {
		t.Fatalf("the group's model deny was dropped next to its route: %+v", d)
	}
	// The same group's deny outranks what its own route allows.
	rule := RouteRule{CredentialIDs: []string{b}, DeniedCredentialIDs: []string{a}}
	if _, err := store.UpdateGroup(GroupPatch{ID: group.ID, Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	if d = store.ResolveRouting(scope, "model", "model"); d.AllowsCredential(a, "auth-files", "codex") || !d.AllowsCredential(b, "auth-files", "codex") {
		t.Fatalf("the group's deny did not override its route: %+v", d)
	}
}

func TestDenyOnlyGroupCountsAsConfigured(t *testing.T) {
	store := newStore(t)
	const key = "dummy-deny-only-group"
	if _, err := store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := CallerScope(key)
	c, x := CredentialFingerprint("dummy-c"), CredentialFingerprint("dummy-x")
	if _, err := store.CreateGroup(KeyGroup{ID: "block", Name: "Block", Rule: RouteRule{DeniedModels: []string{"m"}, DeniedCredentialIDs: []string{x}}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKeyRoutes(scope, RouteBindings{RouteRule: RouteRule{CredentialIDs: []string{c, x}}}); err != nil {
		t.Fatal(err)
	}
	d := store.ResolveRouting(scope, "other", "other")
	if d.AccessDenied != "" || !d.AllowsModel() || !d.AllowsCredential(c, "auth-files", "codex") || d.AllowsCredential(x, "auth-files", "codex") {
		t.Fatalf("a deny-only group was treated as unconfigured or its deny ignored: %+v", d)
	}
	if d = store.ResolveRouting(scope, "m", "m"); d.AllowsModel() {
		t.Fatalf("the deny-only group's model deny was ignored: %+v", d)
	}
}

func TestUnconfiguredGroupsDenyAndDisabledAccessControlIgnoresGroupRules(t *testing.T) {
	store := newStore(t)
	ref := CredentialFingerprint("dummy-ref")
	store.ReplaceAll(func(state *State) {
		state.Groups = []KeyGroup{
			{ID: "empty", Name: "Empty"},
			{ID: "direct", Name: "Direct", Rule: RouteRule{CredentialIDs: []string{ref}, DeniedModels: []string{"blocked"}}},
		}
		state.Keys["empty"] = &KeyState{GroupIDs: []string{"empty"}}
		state.Keys["mixed"] = &KeyState{GroupIDs: []string{"empty", "direct"}}
	})
	const message = "API key groups have no routing rules or upstream credentials configured; access is denied"
	if d := store.ResolveRouting("empty", "model", "model"); d.AccessDenied != message || d.AllowsCredential(ref, "auth-files", "codex") {
		t.Fatalf("unconfigured group decision = %+v", d)
	}
	d := store.ResolveRouting("mixed", "model", "model")
	if d.AccessDenied != "" || !d.AllowsCredential(ref, "auth-files", "codex") || d.AllowsCredential("other", "auth-files", "codex") {
		t.Fatalf("a configured group did not grant alongside an empty one: %+v", d)
	}
	if d = store.ResolveRouting("mixed", "blocked", "blocked"); d.AllowsModel() {
		t.Fatalf("group model deny ignored: %+v", d)
	}
	if err := store.SetAccessControl(AccessControl{DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"empty", "mixed"} {
		d := store.ResolveRouting(scope, "blocked", "blocked")
		if d.AccessDenied != "" || !d.AllowsModel() || d.RestrictsCredentials() || !d.AllowsCredential("other", "auth-files", "codex") {
			t.Fatalf("disabled access control enforced group rules for %s: %+v", scope, d)
		}
	}
}

func TestGroupRuleNormalizesAndNeverAliasesCallerSlices(t *testing.T) {
	ref := CredentialFingerprint("dummy-normalize")
	upper := "sha256:" + strings.ToUpper(strings.TrimPrefix(ref, "sha256:"))
	group, err := NormalizeGroup(KeyGroup{ID: " g ", Name: " G ", Rule: RouteRule{
		Models: []string{" model ", "MODEL"}, CredentialIDs: []string{upper, ref},
		CredentialProviders: []CredentialProviderSelector{{Source: " AI-Providers ", Provider: " Codex "}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := RouteRule{
		Models: []string{"model"}, CredentialIDs: []string{ref},
		CredentialProviders: []CredentialProviderSelector{{Source: CredentialSourceAIProviders, Provider: "codex"}},
		DeniedModels:        []string{}, DeniedCredentialIDs: []string{}, DeniedCredentialProviders: []CredentialProviderSelector{},
	}
	if group.ID != "g" || !reflect.DeepEqual(group.Rule, want) {
		t.Fatalf("normalized group = %+v", group)
	}
	empty, err := NormalizeGroup(KeyGroup{ID: "e", Name: "E"})
	if err != nil || empty.Rule.Models == nil || empty.Rule.DeniedCredentialProviders == nil || !empty.Rule.empty() {
		t.Fatalf("empty rule was not normalized to empty lists: %+v %v", empty, err)
	}
	for _, rule := range []RouteRule{
		{Models: []string{"m"}, DeniedModels: []string{"M"}},
		{CredentialIDs: []string{"not-a-fingerprint"}},
		{CredentialProviders: []CredentialProviderSelector{{Source: "elsewhere", Provider: "codex"}}},
	} {
		if _, err := NormalizeGroup(KeyGroup{ID: "x", Name: "X", Rule: rule}); KindOf(err) != KindInvalid {
			t.Fatalf("invalid rule %+v accepted: %v", rule, err)
		}
	}

	store := newStore(t)
	input := RouteRule{CredentialIDs: []string{ref}, Models: []string{"model"}}
	created, err := store.CreateGroup(KeyGroup{Name: "Alias", Rule: input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	input.CredentialIDs[0], input.Models[0] = "mutated", "mutated"
	created.Rule.CredentialIDs[0], created.Rule.Models[0] = "mutated", "mutated"
	stored, ok := store.Group(created.ID)
	if !ok || !slices.Equal(stored.Rule.CredentialIDs, []string{ref}) || !slices.Equal(stored.Rule.Models, []string{"model"}) {
		t.Fatalf("stored rule aliased caller slices: %+v", stored)
	}
	stored.Rule.CredentialIDs[0] = "mutated"
	views := store.GroupViews()
	views[0].Rule.Models[0] = "mutated"
	if again, _ := store.Group(created.ID); !slices.Equal(again.Rule.CredentialIDs, []string{ref}) || !slices.Equal(again.Rule.Models, []string{"model"}) {
		t.Fatalf("read copies aliased the stored rule: %+v", again)
	}

	name := "Renamed"
	if _, err := store.UpdateGroup(GroupPatch{ID: created.ID, Name: &name}); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.Group(created.ID); again.Name != name || !slices.Equal(again.Rule.CredentialIDs, []string{ref}) {
		t.Fatalf("patch without rule changed the rule: %+v", again)
	}
	cleared := RouteRule{}
	if _, err := store.UpdateGroup(GroupPatch{ID: created.ID, Rule: &cleared}); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.Group(created.ID); !again.Rule.empty() || again.Rule.CredentialIDs == nil {
		t.Fatalf("explicit empty rule was not stored: %+v", again)
	}
	invalid := RouteRule{Models: []string{"m"}, DeniedModels: []string{"m"}}
	if _, err := store.UpdateGroup(GroupPatch{ID: created.ID, Rule: &invalid}); KindOf(err) != KindInvalid {
		t.Fatalf("conflicting rule accepted: %v", err)
	}
	if _, ok := store.Group("missing"); ok {
		t.Fatal("missing group found")
	}
}
