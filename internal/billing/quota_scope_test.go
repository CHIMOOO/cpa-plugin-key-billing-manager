package billing

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func scopedQuotaStore(t *testing.T) (*Store, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "team", Name: "Team", Windows: []QuotaWindow{
			{ID: "astra", Name: "Astra", AmountUSD: 100, PeriodSeconds: 3600, Scope: QuotaScope{Models: []string{"gpt-6-astra"}}},
			{ID: "gpt55", Name: "GPT 5.5", AmountUSD: 200, PeriodSeconds: 3600, Scope: QuotaScope{Models: []string{"gpt-5.5", "route/gpt-5.5"}}},
			{ID: "openai", Name: "OpenAI", AmountUSD: 1000, PeriodSeconds: 3600, Scope: QuotaScope{Providers: []string{"openai"}}},
			{ID: "claude", Name: "Claude", AmountUSD: 200, PeriodSeconds: 3600, Scope: QuotaScope{Providers: []string{"claude"}}},
		}}}
		for _, model := range []string{"gpt-6-astra", "gpt-5.5", "route/gpt-5.5", "claude-sonnet-4-6", "custom-alias"} {
			state.Prices[model] = CustomPrice{ModelID: model, PriceRates: PriceRates{InputPer1M: 1}}
		}
		state.Keys["alice"] = &KeyState{PlanID: "team"}
		state.Keys["bob"] = &KeyState{PlanID: "team"}
	})
	return store, now
}

func spendScoped(t *testing.T, store *Store, scope, model string, dollars int64, now time.Time) {
	t.Helper()
	if d := store.AuthorizeModel(scope, model, model, now); !d.Allowed {
		t.Fatalf("admission %s: %+v", model, d)
	}
	store.RecordUsage(UsageEvent{Scope: scope, UpstreamModel: model, RouteModel: model,
		RequestedAt: now, At: now, Breakdown: completeBreakdown(dollars*1000000, 0, 0, 0, 0)})
}

func TestScopedQuotasHaveIndependentPerKeyBalances(t *testing.T) {
	store, now := scopedQuotaStore(t)
	spendScoped(t, store, "alice", "gpt-6-astra", 100, now)
	if d := store.AuthorizeModel("alice", "gpt-6-astra", "gpt-6-astra", now); d.Allowed || len(d.Windows) != 2 || d.Windows[0].Dimensions[0].Used != "100" {
		t.Fatalf("astra should be exhausted: %+v", d)
	}
	for _, model := range []string{"gpt-5.5", "claude-sonnet-4-6", "grok-4", "custom-alias"} {
		if d := store.AuthorizeModel("alice", model, model, now); !d.Allowed {
			t.Fatalf("independent %s blocked: %+v", model, d)
		}
	}
	if d := store.AuthorizeModel("bob", "gpt-6-astra", "gpt-6-astra", now); !d.Allowed || d.Windows[0].Dimensions[0].Used != "0" {
		t.Fatalf("bob shares alice quota: %+v", d)
	}
	view, _ := store.KeyViewForScope("alice")
	if view.Blocked || !view.PartiallyBlocked {
		t.Fatalf("whole key falsely blocked: %+v", view)
	}
	spendScoped(t, store, "alice", "route/gpt-5.5", 200, now)
	if d := store.AuthorizeModel("alice", "gpt-5.5", "gpt-5.5(high)", now); d.Allowed {
		t.Fatal("same multi-model pool not shared")
	}
	store.read(func(state *State) {
		cycles := state.Keys["alice"].Cycles
		if cycles["astra"].SpentUSD != 100 || cycles["gpt55"].SpentUSD != 200 || cycles["openai"].SpentUSD != 300 || cycles["claude"].SpentUSD != 0 {
			t.Fatalf("independent charges: %+v", cycles)
		}
	})
}

func TestScopedQuotasStackWithoutDuplicatingInvoice(t *testing.T) {
	store, now := scopedQuotaStore(t)
	store.ReplaceAll(func(state *State) {
		state.Plans[0].Windows = append(state.Plans[0].Windows, QuotaWindow{ID: "overall", Name: "Overall", PeriodSeconds: 7200, AmountUSD: 10})
	})
	spendScoped(t, store, "alice", "gpt-5.5", 10, now)
	if d := store.AuthorizeModel("alice", "claude-sonnet-4-6", "claude-sonnet-4-6", now); d.Allowed || !d.Blocked || !d.RetryAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("overall cap not enforced across families: %+v", d)
	}
	page, err := store.RequestEvents(RequestEventQuery{})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Cost.TotalUSD != 10 {
		t.Fatalf("stacked caps duplicate invoice: %+v %v", page, err)
	}
}

func TestQuotaScopesNormalizeAndValidate(t *testing.T) {
	scope := QuotaScope{Models: []string{" GPT-5.5 ", "gpt-6-astra", "gpt-5.5"}}
	normalized, err := scope.normalized()
	if err != nil || !reflect.DeepEqual(normalized.Models, []string{"gpt-5.5", "gpt-6-astra"}) || scope.Models[0] != " GPT-5.5 " {
		t.Fatalf("normalization: %+v %v", normalized, err)
	}
	for _, bad := range []QuotaScope{
		{Models: []string{""}}, {Models: []string{"gpt-*"}}, {Models: []string{"\x00"}},
		{Providers: []string{"unknown"}}, {Providers: []string{""}},
		{Models: []string{"gpt-5.5"}, Providers: []string{"openai"}},
	} {
		if _, err := bad.normalized(); err == nil {
			t.Fatalf("invalid scope accepted: %+v", bad)
		}
	}
	for _, raw := range []string{`{"model":["gpt-5.5"]}`, `{"providers":["made-up"]}`, `{"models":["gpt-5.5"],"providers":["openai"]}`} {
		var scope QuotaScope
		if json.Unmarshal([]byte(raw), &scope) == nil {
			t.Fatalf("malformed stored scope accepted: %s", raw)
		}
	}
	store, _ := scopedQuotaStore(t)
	plan := store.Plans()[0]
	if err := plan.Validate(); err != nil {
		t.Fatalf("same period for different scopes: %v", err)
	}
	duplicate := plan.Windows[1]
	duplicate.ID, duplicate.Name = "dup", "Duplicate"
	duplicate.Scope.Models = []string{"route/gpt-5.5", "GPT-5.5"}
	plan.Windows = append(plan.Windows, duplicate)
	if err := plan.Validate(); err == nil {
		t.Fatal("same canonical scope/period accepted twice")
	}
	copy := store.Plans()
	copy[0].Windows[0].Scope.Models[0] = "mutated"
	if store.Plans()[0].Windows[0].Scope.Models[0] != "gpt-6-astra" {
		t.Fatal("plan view leaks mutable scope")
	}
}

func TestModelFamilyScopeUsesBillingIdentity(t *testing.T) {
	for model, want := range map[string]string{
		"route/GPT-6-ASTRA(high)": "openai", "o3-mini": "openai", "o9": "openai",
		"openai/claude-sonnet-4-6": "claude", "grok-4": "xai", "gemini-3-flash": "gemini",
		"deepseek-v4": "deepseek", "qwen3-max": "qwen", "our-special-alias": "", "other-model": "",
	} {
		if got := quotaModelFamily(model); got != want {
			t.Errorf("family %q = %q, want %q", model, got, want)
		}
	}
	store, now := scopedQuotaStore(t)
	if d := store.AuthorizeModel("alice", "claude-sonnet-4-6", "route/gpt-5.5(high)", now); !d.Allowed {
		t.Fatal(d)
	}
	store.RecordUsage(UsageEvent{Scope: "alice", UpstreamModel: "claude-sonnet-4-6", RouteModel: "route/gpt-5.5(high)",
		Provider: "claude", RequestedAt: now, At: now, Breakdown: completeBreakdown(1000000, 0, 0, 0, 0)})
	store.read(func(state *State) {
		cycles := state.Keys["alice"].Cycles
		if cycles["gpt55"].SpentUSD != 1 || cycles["openai"].SpentUSD != 1 || !cycles["claude"].StartAt.IsZero() {
			t.Fatalf("admission/usage chose different identities: %+v", cycles)
		}
	})
}

func TestExactQuotaScopePreservesConfiguredThinkingIdentity(t *testing.T) {
	store, now := scopedQuotaStore(t)
	store.ReplaceAll(func(state *State) {
		state.Prices["custom(high)"] = CustomPrice{ModelID: "custom(high)", PriceRates: PriceRates{InputPer1M: 1}}
		state.Plans[0].Windows = []QuotaWindow{{ID: "custom", Name: "Custom high", PeriodSeconds: 3600,
			AmountUSD: 1, Scope: QuotaScope{Models: []string{"custom(high)"}}}}
	})
	spendScoped(t, store, "alice", "custom(high)", 1, now)
	if decision := store.AuthorizeModel("alice", "gpt-6-astra", "custom(high)", now); decision.Allowed {
		t.Fatal("configured suffix identity bypasses exact quota")
	}
	if decision := store.AuthorizeModel("alice", "gpt-6-astra", "custom(low)", now); !decision.Allowed {
		t.Fatal("other thinking alias consumes independent configured suffix quota")
	}
}

func TestDynamicAutoRequiresExplicitModelForScopedQuota(t *testing.T) {
	for _, selectedScope := range []QuotaScope{{Models: []string{"gpt-5.5"}}, {Providers: []string{"openai"}}, {}} {
		store, now := scopedQuotaStore(t)
		store.ReplaceAll(func(state *State) {
			state.Plans[0].Windows = []QuotaWindow{{ID: "w", Name: "Quota", PeriodSeconds: 3600, AmountUSD: 100, Scope: selectedScope}}
		})
		for _, request := range [][2]string{{"public-model", "auto"}, {"public-model(high)", "AUTO(high)"}, {"auto", "auto"}, {"auto(high)", ""}} {
			decision := store.AuthorizeModel("alice", request[0], request[1], now)
			if decision.Allowed != selectedScope.IsZero() || decision.ModelUnresolved == selectedScope.IsZero() {
				t.Fatalf("dynamic request %+v scope %+v: %+v", request, selectedScope, decision)
			}
		}
		if !selectedScope.IsZero() {
			store.read(func(state *State) {
				if len(state.Keys["alice"].Cycles) != 0 {
					t.Fatal("refused auto started quota windows")
				}
			})
		}
		for _, model := range []string{"gpt-5.5", "gpt-auto", "my-model"} {
			if decision := store.AuthorizeModel("alice", "gpt-5.5", model, now); !decision.Allowed || decision.ModelUnresolved {
				t.Fatalf("stable alias %s refused: %+v", model, decision)
			}
		}
	}
}

func TestChangingScopeResetsOnlyThatWindowAndRejectsOldUsage(t *testing.T) {
	store, start := scopedQuotaStore(t)
	spendScoped(t, store, "alice", "gpt-5.5", 1, start)
	plan := store.Plans()[0]
	for i := range plan.Windows {
		if plan.Windows[i].ID == "gpt55" {
			plan.Windows[i].Scope.Models = []string{"gpt-6-astra", "gpt-5.5"}
		}
	}
	now := start.Add(time.Minute)
	store.now = func() time.Time { return now }
	if _, err := store.UpdatePlanWithBindings(PlanPatch{ID: plan.ID, Windows: &plan.Windows}, nil); err != nil {
		t.Fatal(err)
	}
	store.AuthorizeModel("alice", "gpt-6-astra", "gpt-6-astra", now)
	store.RecordUsage(UsageEvent{Scope: "alice", UpstreamModel: "gpt-6-astra", RouteModel: "gpt-6-astra",
		RequestedAt: start, At: now, Breakdown: completeBreakdown(1000000, 0, 0, 0, 0)})
	store.read(func(state *State) {
		cycles := state.Keys["alice"].Cycles
		if cycles["gpt55"].SpentUSD != 0 || cycles["openai"].SpentUSD != 2 || !cycles["gpt55"].StartAt.Equal(now) {
			t.Fatalf("scope reset affected unrelated usage or billed old usage: %+v", cycles)
		}
	})
}
