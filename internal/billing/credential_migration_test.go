package billing

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestCredentialMigrationRetainsThenRetiresExactGrantsAndDenials(t *testing.T) {
	s, repo := newStoreWithRepository(t)
	old, next, other := CredentialFingerprint("dummy-old"), CredentialFingerprint("dummy-next"), CredentialFingerprint("dummy-other")
	rule := RouteRule{CredentialIDs: []string{old, other}, DeniedCredentialIDs: []string{old}, Models: []string{"dummy-model"}}
	s.ReplaceAll(func(state *State) {
		state.Routes = []Route{{ID: "route", Rule: rule.clone()}}
		state.Groups = []KeyGroup{{ID: "group", Rule: rule.clone()}}
		state.Keys["dummy-scope"] = &KeyState{RouteBindings: RouteBindings{Configured: true, RouteRule: rule.clone()}}
		state.ConfigCredentials[old] = ConfigCredential{Provider: "dummy-provider", Disabled: true}
	})
	mapping := map[string]string{old: next}
	assertRules := func(state *State, complete bool) {
		t.Helper()
		for _, got := range []RouteRule{state.Routes[0].Rule, state.Groups[0].Rule, state.Keys["dummy-scope"].RouteBindings.RouteRule} {
			if !slices.Contains(got.CredentialIDs, next) || !slices.Contains(got.DeniedCredentialIDs, next) || !slices.Contains(got.CredentialIDs, other) {
				t.Fatalf("migration lost grants/denials: %+v", got)
			}
			if slices.Contains(got.CredentialIDs, old) == complete || slices.Contains(got.DeniedCredentialIDs, old) == complete {
				t.Fatalf("wrong old identity lifecycle: %+v", got)
			}
			if (RoutingDecision{RouteRule: got}).AllowsCredential(next, CredentialSourceAIProviders, "dummy-provider") {
				t.Fatal("replacement bypassed deny precedence")
			}
		}
		if value, ok := state.ConfigCredentials[next]; !ok || !value.Disabled {
			t.Fatal("inventory migration lost disabled state")
		}
		if _, exists := state.ConfigCredentials[old]; exists == complete {
			t.Fatal("wrong configured identity lifecycle")
		}
	}
	for _, complete := range []bool{false, false, true, true} {
		if err := s.MigrateCredentialRefs(mapping, complete); err != nil {
			t.Fatal(err)
		}
		s.Read(func(state *State) { assertRules(state, complete) })
		assertRules(repo.state, complete)
	}
}

func TestCredentialMigrationPersistenceFailureRollsBackEveryRule(t *testing.T) {
	s, repo := newStoreWithRepository(t)
	old, next := CredentialFingerprint("dummy-old"), CredentialFingerprint("dummy-next")
	s.ReplaceAll(func(state *State) {
		rule := RouteRule{CredentialIDs: []string{old}, DeniedCredentialIDs: []string{old}}
		state.Routes = []Route{{ID: "route", Rule: rule.clone()}}
		state.Groups = []KeyGroup{{ID: "group", Rule: rule.clone()}}
		state.Keys["scope"] = &KeyState{RouteBindings: RouteBindings{RouteRule: rule.clone()}}
		state.ConfigCredentials[old] = ConfigCredential{Provider: "dummy"}
	})
	for _, complete := range []bool{false, true} {
		var before []byte
		s.Read(func(state *State) { before, _ = json.Marshal(state) })
		repo.fail = errors.New("dummy disk failure")
		if err := s.MigrateCredentialRefs(map[string]string{old: next}, complete); err == nil {
			t.Fatal("failed migration reported success")
		}
		s.Read(func(state *State) {
			after, _ := json.Marshal(state)
			if string(after) != string(before) {
				t.Fatal("failed transaction mutated live state")
			}
		})
		repo.fail = nil
		if !complete {
			if err := s.MigrateCredentialRefs(map[string]string{old: next}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCredentialMigrationRejectsUnverifiableMappings(t *testing.T) {
	s, _ := newStoreWithRepository(t)
	a, b, c := CredentialFingerprint("dummy-a"), CredentialFingerprint("dummy-b"), CredentialFingerprint("dummy-c")
	for _, mapping := range []map[string]string{{a: a}, {a: b, b: c}, {a: b, b: a}, {"dummy-raw-key": b}} {
		if err := s.MigrateCredentialRefs(mapping, false); err == nil {
			t.Fatal("invalid identity replacement accepted")
		}
	}
}
