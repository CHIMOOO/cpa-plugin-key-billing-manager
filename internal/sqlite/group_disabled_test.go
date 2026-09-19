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

func TestV17GroupDisabledMigrationPreservesDataAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v17.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(strings.TrimSuffix(schema, groupDisabledSchema) + `
PRAGMA user_version=17;
INSERT INTO api_keys(scope,preview,label) VALUES('scope','dum…001','History');
INSERT INTO groups(position,id,name,route_ids_json,rule_json) VALUES(0,'team','Team','["route"]','{"models":["model"]}');
INSERT INTO key_groups(scope,position,group_id) VALUES('scope',0,'team');
INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'scope',0,0),(2,2,'scope',1,0),(3,3,'scope',0,2.5);
INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'preserved');
`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := raw.Exec("ALTER TABLE groups ADD COLUMN disabled TEXT NOT NULL DEFAULT 'keep'"); err != nil {
					t.Fatal(err)
				}
			}
			d, err := Open(path)
			if conflict {
				if err == nil {
					d.Close()
					t.Fatal("conflicting schema accepted")
				}
				var version int
				var value string
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 17 {
					t.Fatalf("migration version did not roll back: %d %v", version, err)
				}
				if err := raw.QueryRow("SELECT disabled FROM groups WHERE id = 'team'").Scan(&value); err != nil || value != "keep" {
					t.Fatalf("existing data lost: %q %v", value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			state := mustLoad(t, d).State
			if len(state.Groups) != 1 || state.Groups[0].Disabled || !reflect.DeepEqual(state.Groups[0].Rule.Models, []string{"model"}) ||
				!reflect.DeepEqual(state.Groups[0].RouteIDs, []string{"route"}) || !reflect.DeepEqual(state.Keys["scope"].GroupIDs, []string{"team"}) || state.Keys["scope"].Label != "History" {
				t.Fatalf("migration changed group configuration: %+v", state)
			}
			events, err := d.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || events.Total != 3 || events.Statuses.Failed != 1 {
				t.Fatalf("history lost: %+v %v", events, err)
			}
			errors, err := d.RequestErrors(billing.RequestErrorQuery{}, time.Time{})
			if err != nil || len(errors.Entries) != 1 || errors.Entries[0].Body != "preserved" {
				t.Fatalf("failure lost: %+v %v", errors, err)
			}
			state.Groups[0].Disabled = true
			mustSave(t, d, state, billing.Changes{Groups: true})
			if loaded := mustLoad(t, d).State.Groups; !reflect.DeepEqual(loaded, state.Groups) {
				t.Fatalf("disabled group round trip = %+v", loaded)
			}
		})
	}
}
