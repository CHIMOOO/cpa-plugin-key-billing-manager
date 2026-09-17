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
	state.Groups = []billing.KeyGroup{{ID: "a", Name: "A", RouteIDs: []string{"route"}}, {ID: "b", Name: "B", RouteIDs: []string{}}}
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
			oldSchema := strings.TrimSuffix(schema, accessControlSchema)
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
				if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('access_control', 'key_groups')").Scan(&tables); err != nil || tables != 0 {
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
