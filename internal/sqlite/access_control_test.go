package sqlite

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestAccessControlRoundTripAndAtomicFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.db")
	d := openDatabase(t, path)
	state := billing.NewState()
	state.AccessControl = billing.AccessControl{Enabled: false, DenyUngrouped: true}
	for _, group := range []billing.KeyGroup{
		{ID: "a", Name: "A", RouteIDs: []string{"route"}},
		{ID: "b", Name: "B", Disabled: true, RouteIDs: []string{}, Rule: billing.RouteRule{
			Models: []string{"model"}, CredentialIDs: []string{billing.CredentialFingerprint("dummy-direct")},
			CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAIProviders, Provider: "codex"}},
			DeniedModels:        []string{"blocked"}, DeniedCredentialIDs: []string{billing.CredentialFingerprint("dummy-denied")},
			DeniedCredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "claude"}},
		}},
	} {
		normalized, err := billing.NormalizeGroup(group)
		if err != nil {
			t.Fatal(err)
		}
		state.Groups = append(state.Groups, normalized)
	}
	state.Routes = []billing.Route{{ID: "route", Name: "Route", Rule: billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-credential")}}}}
	state.Keys["scope"] = &billing.KeyState{Preview: "dum…001", GroupIDs: []string{"a", "b"}, RouteBindings: billing.RouteBindings{Configured: true}}
	changes := billing.Changes{AllKeys: true, Groups: true, Routes: true, AccessControl: true}
	mustSave(t, d, state, changes)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = openDatabase(t, path)
	loaded := mustLoad(t, d).State
	if loaded.AccessControl != state.AccessControl || !reflect.DeepEqual(loaded.Groups, state.Groups) || !reflect.DeepEqual(loaded.Keys["scope"].GroupIDs, state.Keys["scope"].GroupIDs) {
		t.Fatalf("policy not persisted: %+v", loaded)
	}
	if !loaded.Keys["scope"].RouteBindings.Configured {
		t.Fatal("explicit empty direct selection was not persisted")
	}
	var rule string
	if err := d.db.QueryRow("SELECT rule_json FROM groups WHERE id = 'a'").Scan(&rule); err != nil ||
		rule != `{"models":[],"credential_ids":[],"credential_providers":[],"denied_models":[],"denied_credential_ids":[],"denied_credential_providers":[]}` {
		t.Fatalf("empty group rule stored as %q (%v)", rule, err)
	}
	if _, err := d.db.Exec("CREATE TRIGGER fail_group_write BEFORE INSERT ON groups BEGIN SELECT RAISE(ABORT, 'dummy failure'); END"); err != nil {
		t.Fatal(err)
	}
	state.Keys["scope"].GroupIDs = []string{"b"}
	state.AccessControl.Enabled = true
	if err := d.Save(state, changes); err == nil {
		t.Fatal("expected transactional failure")
	}
	after := mustLoad(t, d).State
	if !reflect.DeepEqual(loaded, after) {
		t.Fatal("failed group save committed partial membership or settings changes")
	}
}

func TestV14AccessControlMigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v14.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			oldSchema := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(schema, groupRoutingModeSchema), groupDisabledSchema), groupRuleSchema), forwardedForBlockSchema), accessControlSchema)
			if oldSchema == schema || strings.Contains(oldSchema, "forwarded_for_block") || strings.Contains(oldSchema, "access_control") {
				t.Fatal("v14 fixture still contains later tables")
			}
			if _, err := raw.Exec(oldSchema + `
PRAGMA user_version=14;
INSERT INTO api_keys(scope,preview,label,route_bindings_json) VALUES('scope','dum…001','History','{"route_ids":["legacy"]}');
INSERT INTO routes(position,id,name,rule_json) VALUES(0,'legacy','Legacy','{"models":["model"]}');
INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'scope',0,0),(2,2,'scope',1,0),(3,3,'scope',0,2.5);
INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'preserved');
`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := raw.Exec("CREATE TABLE groups(marker TEXT); INSERT INTO groups VALUES('preserve')"); err != nil {
					t.Fatal(err)
				}
			}
			d, err := Open(path)
			if conflict {
				if err == nil {
					d.Close()
					t.Fatal("conflicting schema accepted")
				}
				var version, tables int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
					t.Fatalf("migration version did not roll back: %d %v", version, err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('access_control', 'key_groups', 'forwarded_for_block')").Scan(&tables); err != nil || tables != 0 {
					t.Fatalf("partial schema committed: %d %v", tables, err)
				}
				var marker string
				if err := raw.QueryRow("SELECT marker FROM groups").Scan(&marker); err != nil || marker != "preserve" {
					t.Fatal("existing table lost", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			state := mustLoad(t, d).State
			if !state.AccessControl.Enabled || state.AccessControl.DenyUngrouped || len(state.Groups) != 0 || state.Keys["scope"].Label != "History" || len(state.Keys["scope"].RouteBindings.RouteIDs) != 1 {
				t.Fatalf("migration changed legacy config: %+v", state)
			}
			if !reflect.DeepEqual(state.ForwardedForBlock, billing.DefaultForwardedForBlock()) {
				t.Fatalf("v14 migration did not add the disabled X-Forwarded-For block: %+v", state.ForwardedForBlock)
			}
			var ruleColumns int
			if err := d.db.QueryRow("SELECT count(*) FROM pragma_table_info('groups') WHERE name = 'rule_json'").Scan(&ruleColumns); err != nil || ruleColumns != 1 {
				t.Fatalf("v14 migration did not reach the v17 group table: %d %v", ruleColumns, err)
			}
			events, err := d.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || events.Total != 3 || events.Statuses.Failed != 1 {
				t.Fatalf("history lost: %+v %v", events, err)
			}
			errors, err := d.RequestErrors(billing.RequestErrorQuery{}, time.Time{})
			if err != nil || len(errors.Entries) != 1 || errors.Entries[0].Body != "preserved" {
				t.Fatalf("failure lost: %+v %v", errors, err)
			}
		})
	}
}

func TestGroupRuleColumnDefaultsToEmptySelection(t *testing.T) {
	d := openTestDB(t)
	var columnDefault string
	if err := d.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('groups') WHERE name = 'rule_json' AND "notnull" = 1`).Scan(&columnDefault); err != nil || columnDefault != "'{}'" {
		t.Fatalf("rule_json column default = %q (%v)", columnDefault, err)
	}
	if _, err := d.db.Exec(`INSERT INTO groups(position, id, name, route_ids_json) VALUES (0, 'legacy', 'Legacy', '[]')`); err != nil {
		t.Fatal(err)
	}
	groups := mustLoad(t, d).State.Groups
	empty := billing.RouteRule{
		Models: []string{}, CredentialIDs: []string{}, CredentialProviders: []billing.CredentialProviderSelector{},
		DeniedModels: []string{}, DeniedCredentialIDs: []string{}, DeniedCredentialProviders: []billing.CredentialProviderSelector{},
	}
	if len(groups) != 1 || !reflect.DeepEqual(groups[0].Rule, empty) || !reflect.DeepEqual(groups[0].RouteIDs, []string{}) {
		t.Fatalf("default rule loaded as %+v", groups)
	}
	for _, corrupt := range []string{`'[]'`, `'{"models":["m"],"denied_models":["m"]}'`, `'{"credential_ids":["not-a-fingerprint"]}'`} {
		if _, err := d.db.Exec("UPDATE groups SET rule_json = " + corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Load(time.Time{}, time.Time{}); err == nil {
			t.Fatalf("corrupt group rule %s loaded", corrupt)
		}
	}
}

func TestV16GroupRuleMigrationPreservesDataAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v16.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			oldSchema := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(schema, groupRoutingModeSchema), groupDisabledSchema), groupRuleSchema)
			if oldSchema == schema || !strings.Contains(oldSchema, "forwarded_for_block") || strings.Contains(oldSchema, "ALTER TABLE groups") {
				t.Fatal("v16 fixture is not the v16 schema")
			}
			if _, err := raw.Exec(oldSchema + `
PRAGMA user_version=16;
UPDATE access_control SET enabled = 1, deny_ungrouped = 1 WHERE id = 1;
UPDATE forwarded_for_block SET enabled = 1, model_keywords_json = '["gpt"]', message = 'kept' WHERE id = 1;
INSERT INTO api_keys(scope,preview,label,route_bindings_json) VALUES('scope','dum…001','History','{"route_ids":["legacy"]}');
INSERT INTO routes(position,id,name,rule_json) VALUES(0,'legacy','Legacy','{"models":["model"]}');
INSERT INTO groups(position,id,name,route_ids_json) VALUES(0,'team','Team','["legacy"]'),(1,'empty','Empty','[]');
INSERT INTO key_groups(scope,position,group_id) VALUES('scope',0,'team'),('scope',1,'empty');
INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'scope',0,0),(2,2,'scope',1,0),(3,3,'scope',0,2.5);
INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'preserved');
`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				// The column this migration adds already exists: fail and keep everything.
				if _, err := raw.Exec("ALTER TABLE groups ADD COLUMN rule_json TEXT; UPDATE groups SET rule_json = 'preserve'"); err != nil {
					t.Fatal(err)
				}
			}
			d, err := Open(path)
			if conflict {
				if err == nil {
					d.Close()
					t.Fatal("conflicting schema accepted")
				}
				var version, groups int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 16 {
					t.Fatalf("migration version did not roll back: %d %v", version, err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM groups WHERE rule_json = 'preserve'").Scan(&groups); err != nil || groups != 2 {
					t.Fatal("existing data lost", groups, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var version int
			if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion || schemaVersion != 20 {
				t.Fatalf("schema version = %d (%d), err = %v", version, schemaVersion, err)
			}
			state := mustLoad(t, d).State
			if state.AccessControl != (billing.AccessControl{Enabled: true, DenyUngrouped: true}) ||
				!reflect.DeepEqual(state.ForwardedForBlock, billing.ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}, Message: "kept"}) ||
				len(state.Groups) != 2 || !reflect.DeepEqual(state.Groups[0].RouteIDs, []string{"legacy"}) ||
				!reflect.DeepEqual(state.Keys["scope"].GroupIDs, []string{"team", "empty"}) || state.Keys["scope"].Label != "History" ||
				len(state.Keys["scope"].RouteBindings.RouteIDs) != 1 || len(state.Routes) != 1 {
				t.Fatalf("migration changed existing configuration: %+v", state)
			}
			for _, group := range state.Groups {
				rule := group.Rule
				if rule.Models == nil || rule.DeniedCredentialProviders == nil ||
					len(rule.Models)+len(rule.CredentialIDs)+len(rule.CredentialProviders)+
						len(rule.DeniedModels)+len(rule.DeniedCredentialIDs)+len(rule.DeniedCredentialProviders) != 0 {
					t.Fatalf("existing group %s gained a selection: %+v", group.ID, rule)
				}
			}
			events, err := d.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || events.Total != 3 || events.Statuses.Failed != 1 {
				t.Fatalf("history lost: %+v %v", events, err)
			}
			errors, err := d.RequestErrors(billing.RequestErrorQuery{}, time.Time{})
			if err != nil || len(errors.Entries) != 1 || errors.Entries[0].Body != "preserved" {
				t.Fatalf("failure lost: %+v %v", errors, err)
			}

			// The migrated table stores and returns a direct selection.
			group, err := billing.NormalizeGroup(billing.KeyGroup{ID: "empty", Name: "Empty", Rule: billing.RouteRule{
				CredentialIDs: []string{billing.CredentialFingerprint("dummy-migrated")},
			}})
			if err != nil {
				t.Fatal(err)
			}
			state.Groups[1] = group
			mustSave(t, d, state, billing.Changes{Groups: true})
			if loaded := mustLoad(t, d).State.Groups; !reflect.DeepEqual(loaded, state.Groups) {
				t.Fatalf("migrated group rule round trip = %+v", loaded)
			}
		})
	}
}
