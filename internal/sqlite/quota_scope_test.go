package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestScopedPlanAndUsagePersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scoped.db")
	database := openDatabase(t, path)
	now := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	state := billing.NewState()
	state.Plans = []billing.Plan{{ID: "p", Windows: []billing.QuotaWindow{
		{ID: "gpt", Name: "GPT", PeriodSeconds: 3600, AmountUSD: 100, Scope: billing.QuotaScope{Models: []string{"gpt-6-astra"}}},
		{ID: "claude", Name: "Claude", PeriodSeconds: 3600, AmountUSD: 200, Scope: billing.QuotaScope{Providers: []string{"claude"}}},
	}}}
	state.Keys["scope"] = &billing.KeyState{Preview: "sk-dum…0001", PlanID: "p", Cycles: map[string]billing.QuotaCycle{
		"gpt": {PlanID: "p", StartAt: now, EndAt: now.Add(time.Hour), SpentUSD: 3.5, UsedRequests: 2, UsedTokens: 100},
	}}
	mustSave(t, database, state, billing.Changes{Plans: true, AllKeys: true,
		NormalRequestEvents: []billing.RequestEvent{{At: now, Scope: "scope", BillingModel: "gpt-6-astra"}},
		RequestErrorEvents:  []billing.RequestErrorEvent{{Event: billing.RequestEvent{At: now, Scope: "scope", Failed: true}}},
	})
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openDatabase(t, path)
	loaded := mustLoad(t, reopened)
	if !reflect.DeepEqual(loaded.State.Plans, state.Plans) || !reflect.DeepEqual(loaded.State.Keys["scope"].Cycles, state.Keys["scope"].Cycles) || loaded.RequestEventCount != 2 {
		t.Fatalf("scope/balance/history lost: %+v", loaded)
	}
}

func TestV19MigrationPreservesLegacyBalancesAndHistoryAtomically(t *testing.T) {
	for _, corrupt := range []string{"", "plan", "cycles"} {
		t.Run(corrupt, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(strings.TrimSuffix(schema, groupRoutingModeSchema) + `PRAGMA user_version=18;
INSERT INTO plans(position,id,name,windows_json) VALUES(0,'p','Plan','[{"id":"w", "name":"Quota", "period_seconds":3600,"amount_usd":100}]');
INSERT INTO api_keys(scope,preview,plan_id,cycles_json) VALUES('scope','sk-dum…0001','p','{"w":{"plan_id":"p", "start_at":"2026-09-19T09:00:00Z","end_at":"2026-09-19T10:00:00Z","spent_usd":7.5}}');
INSERT INTO request_events(at,scope,failed) VALUES(1,'scope',1),(2,'scope',0);`); err != nil {
				t.Fatal(err)
			}
			if corrupt == "plan" {
				_, err = raw.Exec(`UPDATE plans SET windows_json='[{"id":"w","name":"Quota","period_seconds":0,"amount_usd":100}]'`)
			} else if corrupt == "cycles" {
				_, err = raw.Exec(`UPDATE api_keys SET cycles_json='null'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			var oldPlan, oldCycles string
			raw.QueryRow("SELECT windows_json FROM plans").Scan(&oldPlan)
			raw.QueryRow("SELECT cycles_json FROM api_keys").Scan(&oldCycles)
			database, err := Open(path)
			if corrupt != "" && err == nil || corrupt == "" && err != nil {
				t.Fatalf("migration corrupt=%q: %v", corrupt, err)
			}
			if database != nil {
				database.Close()
			}
			var plan, cycles string
			var version, events, failures int
			raw.QueryRow("SELECT windows_json FROM plans").Scan(&plan)
			raw.QueryRow("SELECT cycles_json FROM api_keys").Scan(&cycles)
			raw.QueryRow("PRAGMA user_version").Scan(&version)
			raw.QueryRow("SELECT count(*),sum(failed) FROM request_events").Scan(&events, &failures)
			wantVersion := schemaVersion
			if corrupt != "" {
				wantVersion = 18
			}
			if plan != oldPlan || cycles != oldCycles || events != 2 || failures != 1 || version != wantVersion {
				t.Fatalf("migration changed history or did not roll back: version=%d plan=%s cycles=%s events=%d failures=%d", version, plan, cycles, events, failures)
			}
			if corrupt == "" {
				var windows []billing.QuotaWindow
				if err := json.Unmarshal([]byte(plan), &windows); err != nil || !windows[0].Scope.IsZero() {
					t.Fatalf("legacy scope changed: %+v %v", windows, err)
				}
			}
		})
	}
}
