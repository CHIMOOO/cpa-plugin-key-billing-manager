package billing

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestExclusiveGroupsBoundCommonAndDirectConstraints(t *testing.T) {
	a, b, c, outside := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b"), CredentialFingerprint("dummy-c"), CredentialFingerprint("dummy-outside")
	state := NewState()
	state.Groups = []KeyGroup{
		{ID: "ordinary", Name: "Ordinary", RouteIDs: []string{"missing-ordinary-route"}, Rule: RouteRule{CredentialIDs: []string{outside}, DeniedCredentialIDs: []string{a}}},
		{ID: "exclusive", Name: "Exclusive", RoutingMode: GroupRoutingExclusive, Rule: RouteRule{Models: []string{"a", "b"}, CredentialIDs: []string{a, b}}},
		{ID: "common", Name: "Common", RoutingMode: GroupRoutingCommon, Rule: RouteRule{Models: []string{"c"}, CredentialIDs: []string{c}, DeniedCredentialIDs: []string{b}}},
		{ID: "unbound-common", Name: "Unbound", RoutingMode: GroupRoutingCommon, Rule: RouteRule{CredentialIDs: []string{outside}}},
		{ID: "disabled-exclusive", Name: "Disabled", Disabled: true, RoutingMode: GroupRoutingExclusive, RouteIDs: []string{"missing-disabled-route"}},
	}
	key := &KeyState{GroupIDs: []string{"ordinary", "exclusive", "common", "disabled-exclusive"}}
	d := resolveRoutingState(state, key)
	if d.ConfigurationError != "" || !d.AllowsCredential(a, "auth-files", "codex") || d.AllowsCredential(b, "auth-files", "codex") || !d.AllowsCredential(c, "auth-files", "codex") || d.AllowsCredential(outside, "auth-files", "codex") {
		t.Fatalf("exclusive/common policy = %+v", d)
	}
	// A directly bound route and direct rule remain constraints, never another
	// source of credentials or models outside the selected group union.
	state.Routes = []Route{{ID: "direct", Name: "Direct", Rule: RouteRule{Models: []string{"b", "outside"}, CredentialIDs: []string{outside, b}}}}
	key.RouteBindings = RouteBindings{RouteIDs: []string{"direct"}, RouteRule: RouteRule{Models: []string{"a"}, CredentialIDs: []string{a}}}
	d = resolveRoutingState(state, key)
	if !slices.Equal(d.Models, []string{"a", "b"}) || !d.AllowsCredential(a, "auth-files", "codex") || d.AllowsCredential(b, "auth-files", "codex") || d.AllowsCredential(c, "auth-files", "codex") || d.AllowsCredential(outside, "auth-files", "codex") {
		t.Fatalf("direct selection expanded or lost group constraints: %+v", d)
	}
	key.RouteBindings = RouteBindings{RouteRule: RouteRule{CredentialProviders: []CredentialProviderSelector{{Source: "auth-files", Provider: "codex"}}}}
	d = resolveRoutingState(state, key)
	if !d.AllowsCredential(a, "auth-files", "codex") || d.AllowsCredential(a, "auth-files", "claude") || d.AllowsCredential(outside, "auth-files", "codex") {
		t.Fatalf("provider intersection escaped group pool: %+v", d)
	}
	key.RouteBindings = RouteBindings{RouteRule: RouteRule{Models: []string{"outside"}}}
	d = resolveRoutingState(state, key)
	if d.AccessDenied == "" || d.AllowsModel() || d.AllowsCredential(a, "auth-files", "codex") {
		t.Fatalf("empty model intersection became unrestricted: %+v", d)
	}
	// Missing group identities cannot safely be classified as ordinary/common.
	key.GroupIDs = append(key.GroupIDs, "missing-group")
	if got := resolveRoutingState(state, key); got.ConfigurationError == "" {
		t.Fatal("missing membership bypassed validation")
	}
}

func TestExclusiveGroupChangesAreAtomicAndRestoreOrdinaryGroups(t *testing.T) {
	store := newStore(t)
	scope := CallerScope("dummy-exclusive")
	otherScope := CallerScope("dummy-other")
	if _, err := store.SyncKeys([]string{"dummy-exclusive", "dummy-other"}, false); err != nil {
		t.Fatal(err)
	}
	a, b := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b")
	ordinary, err := store.CreateGroup(KeyGroup{Name: "Ordinary", Rule: RouteRule{CredentialIDs: []string{a}}}, []string{scope})
	if err != nil || ordinary.RoutingMode != GroupRoutingOrdinary {
		t.Fatalf("legacy/default mode: %+v %v", ordinary, err)
	}
	exclusive, err := store.CreateGroup(KeyGroup{Name: "Exclusive", RoutingMode: GroupRoutingExclusive, Rule: RouteRule{CredentialIDs: []string{b}}}, []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateGroup(KeyGroup{Name: "Second", RoutingMode: GroupRoutingExclusive, Disabled: true, Rule: RouteRule{CredentialIDs: []string{a}}}, []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	before := store.GroupViews()
	if _, err := store.CreateGroup(KeyGroup{Name: "Conflict", RoutingMode: GroupRoutingExclusive}, []string{scope}); KindOf(err) != KindConflict {
		t.Fatalf("conflicting creation = %v", err)
	}
	on := false
	if _, err := store.UpdateGroup(GroupPatch{ID: other.ID, Disabled: &on}); KindOf(err) != KindConflict {
		t.Fatalf("conflicting enable = %v", err)
	}
	exclusiveMode := GroupRoutingExclusive
	if _, err := store.UpdateGroup(GroupPatch{ID: ordinary.ID, RoutingMode: &exclusiveMode}); KindOf(err) != KindConflict {
		t.Fatalf("conflicting mode edit = %v", err)
	}
	if !reflect.DeepEqual(before, store.GroupViews()) {
		t.Fatal("failed edits changed saved groups")
	}
	unbound, err := store.CreateGroup(KeyGroup{Name: "Unbound", RoutingMode: GroupRoutingExclusive}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetKeyGroups([]string{scope, otherScope}, []string{exclusive.ID, unbound.ID}); KindOf(err) != KindConflict {
		t.Fatalf("bulk conflict = %v", err)
	}
	otherKey, _ := store.KeyViewForScope(otherScope)
	if len(otherKey.GroupIDs) != 0 {
		t.Fatal("bulk conflict partly changed another key")
	}
	scopes := []string{scope}
	if _, err := store.UpdateGroup(GroupPatch{ID: unbound.ID, Scopes: &scopes}); KindOf(err) != KindConflict {
		t.Fatalf("membership conflict = %v", err)
	}
	d := store.ResolveRouting(scope, "m", "m")
	if d.AllowsCredential(a, "auth-files", "codex") || !d.AllowsCredential(b, "auth-files", "codex") {
		t.Fatalf("unique pool = %+v", d)
	}
	off := true
	if _, err := store.UpdateGroup(GroupPatch{ID: exclusive.ID, Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	d = store.ResolveRouting(scope, "m", "m")
	if !d.AllowsCredential(a, "auth-files", "codex") || d.AllowsCredential(b, "auth-files", "codex") {
		t.Fatalf("ordinary pool not restored after disable: %+v", d)
	}
	if _, err := store.UpdateGroup(GroupPatch{ID: exclusive.ID, Disabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(exclusive.ID); err != nil {
		t.Fatal(err)
	}
	d = store.ResolveRouting(scope, "m", "m")
	if !d.AllowsCredential(a, "auth-files", "codex") {
		t.Fatal("delete failed to restore ordinary group")
	}
}

func TestGroupModesValidateAndCorruptExclusiveMembershipFailsClosed(t *testing.T) {
	for _, mode := range []string{"", "ordinary", "exclusive", "common", " common "} {
		if _, err := NormalizeGroup(KeyGroup{ID: "g", Name: "G", RoutingMode: mode}); err != nil {
			t.Fatal(mode, err)
		}
	}
	if _, err := NormalizeGroup(KeyGroup{ID: "g", Name: "G", RoutingMode: "unrestricted"}); KindOf(err) != KindInvalid {
		t.Fatal(err)
	}
	state := NewState()
	state.Groups = []KeyGroup{{ID: "a", RoutingMode: GroupRoutingExclusive}, {ID: "b", RoutingMode: GroupRoutingExclusive}}
	d := resolveRoutingState(state, &KeyState{GroupIDs: []string{"a", "b"}})
	if !strings.Contains(d.ConfigurationError, "only one enabled exclusive group") || d.AllowsModel() || d.AllowsCredential("ref", "auth-files", "codex") {
		t.Fatalf("legacy conflict did not fail closed: %+v", d)
	}
	state.Groups[1].Disabled = true
	d = resolveRoutingState(state, &KeyState{GroupIDs: []string{"a", "a", "b"}})
	if d.ConfigurationError != "" {
		t.Fatalf("duplicate id or disabled group counted twice: %+v", d)
	}
}

func TestCommonGroupsWithoutExclusiveKeepOrdinaryUnion(t *testing.T) {
	a, b, c := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b"), CredentialFingerprint("dummy-direct")
	state := NewState()
	state.Groups = []KeyGroup{
		{ID: "ordinary", Rule: RouteRule{Models: []string{"ordinary"}, CredentialIDs: []string{a}}},
		{ID: "common", RoutingMode: GroupRoutingCommon, Rule: RouteRule{Models: []string{"common"}, CredentialIDs: []string{b}}},
	}
	key := &KeyState{GroupIDs: []string{"ordinary", "common"}, RouteBindings: RouteBindings{RouteRule: RouteRule{Models: []string{"direct"}, CredentialIDs: []string{c}}}}
	d := resolveRoutingState(state, key)
	if d.CredentialConstraint != nil || !slices.Equal(d.Models, []string{"common", "direct", "ordinary"}) {
		t.Fatalf("no-exclusive policy changed: %+v", d)
	}
	for _, ref := range []string{a, b, c} {
		if !d.AllowsCredential(ref, "auth-files", "codex") {
			t.Fatalf("ordinary union lost %s", ref)
		}
	}
	key.GroupIDs = []string{"ordinary"}
	if resolveRoutingState(state, key).AllowsCredential(b, "auth-files", "codex") {
		t.Fatal("unbound common group leaked global permissions")
	}
}

func TestExclusiveGroupExplicitEmptyDirectAllowlistFailsClosed(t *testing.T) {
	a, b := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b")
	state := NewState()
	state.Groups = []KeyGroup{
		{ID: "exclusive", RoutingMode: GroupRoutingExclusive, Rule: RouteRule{Models: []string{"model"}, CredentialIDs: []string{a}}},
		{ID: "common", RoutingMode: GroupRoutingCommon, Rule: RouteRule{CredentialIDs: []string{b}}},
	}
	state.Routes = []Route{{ID: "direct", Rule: RouteRule{CredentialIDs: []string{b}}}}
	for _, test := range []struct {
		name     string
		bindings RouteBindings
		allowA   bool
		allowB   bool
	}{
		{name: "untouched", allowA: true, allowB: true},
		{name: "explicit-empty", bindings: RouteBindings{Configured: true}},
		{name: "models-only", bindings: RouteBindings{Configured: true, RouteRule: RouteRule{Models: []string{"model"}}}},
		{name: "deny-only", bindings: RouteBindings{Configured: true, RouteRule: RouteRule{DeniedCredentialIDs: []string{a}}}},
		{name: "direct-route", bindings: RouteBindings{Configured: true, RouteIDs: []string{"direct"}}, allowB: true},
		{name: "direct-route-denied", bindings: RouteBindings{Configured: true, RouteIDs: []string{"direct"}, RouteRule: RouteRule{DeniedCredentialIDs: []string{b}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := &KeyState{GroupIDs: []string{"exclusive", "common"}, RouteBindings: test.bindings}
			decision := resolveRoutingState(state, key)
			if decision.ConfigurationError != "" || decision.AllowsCredential(a, "auth-files", "codex") != test.allowA || decision.AllowsCredential(b, "auth-files", "codex") != test.allowB {
				t.Fatalf("configured direct allowlist escaped: %+v", decision)
			}
			if test.bindings.Configured && decision.CredentialConstraint == nil {
				t.Fatal("explicit direct settings were treated as absent")
			}
		})
	}
}

func TestGroupWithoutModelsServesEveryModelBesideListedGroups(t *testing.T) {
	codex, xai := CredentialFingerprint("dummy-codex"), CredentialFingerprint("dummy-xai")
	xaiProvider := []CredentialProviderSelector{{Source: CredentialSourceAIProviders, Provider: "xai"}}
	for _, mode := range []string{GroupRoutingExclusive, GroupRoutingOrdinary} {
		t.Run(mode, func(t *testing.T) {
			store := newStore(t)
			store.ReplaceAll(func(state *State) {
				state.Groups = []KeyGroup{
					{ID: "listed", Name: "Listed", RoutingMode: mode, Rule: RouteRule{Models: []string{"gpt-5"}, CredentialIDs: []string{codex}}},
					{ID: "common", Name: "Common", RoutingMode: GroupRoutingCommon, Rule: RouteRule{CredentialProviders: xaiProvider}},
				}
				state.Keys["scope"] = &KeyState{GroupIDs: []string{"listed", "common"}}
			})
			d := store.ResolveRouting("scope", "grok-4", "grok-4")
			if !d.AllowsModel() || d.AllowsCredential(codex, CredentialSourceAuthFiles, "codex") || !d.AllowsCredential(xai, CredentialSourceAIProviders, "xai") {
				t.Fatalf("unlisted group did not serve its own model: %+v", d)
			}
			d = store.ResolveRouting("scope", "gpt-5", "gpt-5")
			if !d.AllowsModel() || !d.AllowsCredential(codex, CredentialSourceAuthFiles, "codex") {
				t.Fatalf("listed model lost its group: %+v", d)
			}
			if d = store.ResolveRouting("scope", "", ""); d.RestrictsModels() {
				t.Fatalf("summary still lists only the listed group's models: %+v", d)
			}
			// A model denied anywhere stays denied.
			store.ReplaceAll(func(state *State) { state.Groups[0].Rule.DeniedModels = []string{"grok-4"} })
			if store.ResolveRouting("scope", "grok-4", "grok-4").AllowsModel() {
				t.Fatal("model deny bypassed by an unlisted group")
			}
		})
	}
}

func TestModelOnlyGrantsAndDirectModelRulesStillLimitUnlistedGroups(t *testing.T) {
	codex := CredentialFingerprint("dummy-codex")
	xaiProvider := []CredentialProviderSelector{{Source: CredentialSourceAIProviders, Provider: "xai"}}
	store := newStore(t)
	store.ReplaceAll(func(state *State) {
		state.Groups = []KeyGroup{
			{ID: "unique", Name: "Unique", RoutingMode: GroupRoutingExclusive, Rule: RouteRule{Models: []string{"gpt-5"}, CredentialIDs: []string{codex}}},
			{ID: "common", Name: "Common", RoutingMode: GroupRoutingCommon, Rule: RouteRule{CredentialProviders: xaiProvider}},
			{ID: "models", Name: "Models", Rule: RouteRule{Models: []string{"gpt-5"}}},
			{ID: "creds", Name: "Credentials", Rule: RouteRule{CredentialProviders: xaiProvider}},
		}
		// The key's own model rule narrows an exclusive pool, unlisted grants included.
		state.Keys["direct"] = &KeyState{GroupIDs: []string{"unique", "common"}, RouteBindings: RouteBindings{RouteRule: RouteRule{Models: []string{"gpt-5", "grok-4"}}}}
		// A group listing only models limits the models of every other group.
		state.Keys["model-only"] = &KeyState{GroupIDs: []string{"models", "creds"}}
	})
	if d := store.ResolveRouting("direct", "grok-4", "grok-4"); !d.AllowsModel() || !d.AllowsCredential(CredentialFingerprint("dummy-xai"), CredentialSourceAIProviders, "xai") || d.AllowsCredential(codex, CredentialSourceAuthFiles, "codex") {
		t.Fatalf("direct model rule did not admit the unlisted grant: %+v", d)
	}
	if d := store.ResolveRouting("direct", "grok-3", "grok-3"); d.AllowsModel() {
		t.Fatalf("direct model rule was bypassed: %+v", d)
	}
	if d := store.ResolveRouting("model-only", "grok-4", "grok-4"); d.AllowsModel() {
		t.Fatalf("model-only group no longer limits models: %+v", d)
	}
}
