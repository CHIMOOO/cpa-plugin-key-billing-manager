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

func TestV19GroupRoutingModeMigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v19.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(strings.TrimSuffix(schema, groupRoutingModeSchema) + `
PRAGMA user_version=19;
INSERT INTO api_keys(scope,preview,label) VALUES('scope','dum…001','History');
INSERT INTO groups(position,id,name,disabled,route_ids_json,rule_json) VALUES(0,'team','Team',0,'[]','{"models":["model"]}'),(1,'off','Off',1,'[]','{}');
INSERT INTO key_groups(scope,position,group_id) VALUES('scope',0,'team'),('scope',1,'off');
INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'scope',0,0),(2,2,'scope',1,0),(3,3,'scope',0,2.5);
INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'preserved');
`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := raw.Exec("ALTER TABLE groups ADD COLUMN routing_mode TEXT NOT NULL DEFAULT 'preserve'"); err != nil {
					t.Fatal(err)
				}
			}
			d, err := Open(path)
			if conflict {
				if err == nil {
					d.Close()
					t.Fatal("conflicting column accepted")
				}
				var version int
				var value string
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 19 {
					t.Fatalf("version not rolled back: %d %v", version, err)
				}
				if err := raw.QueryRow("SELECT routing_mode FROM groups WHERE id='team'").Scan(&value); err != nil || value != "preserve" {
					t.Fatalf("column data changed: %s %v", value, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			state := mustLoad(t, d).State
			if len(state.Groups) != 2 || state.Groups[0].RoutingMode != billing.GroupRoutingOrdinary || state.Groups[1].RoutingMode != billing.GroupRoutingOrdinary || !state.Groups[1].Disabled || !reflect.DeepEqual(state.Keys["scope"].GroupIDs, []string{"team", "off"}) {
				t.Fatalf("migration changed configuration: %+v", state.Groups)
			}
			events, err := d.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || events.Total != 3 || events.Statuses.Failed != 1 {
				t.Fatalf("history lost: %+v %v", events, err)
			}
			errors, err := d.RequestErrors(billing.RequestErrorQuery{}, time.Time{})
			if err != nil || len(errors.Entries) != 1 || errors.Entries[0].Body != "preserved" {
				t.Fatalf("failure lost: %+v %v", errors, err)
			}
			state.Groups[0].RoutingMode = billing.GroupRoutingExclusive
			state.Groups[1].RoutingMode = billing.GroupRoutingCommon
			mustSave(t, d, state, billing.Changes{Groups: true})
			if loaded := mustLoad(t, d).State.Groups; !reflect.DeepEqual(loaded, state.Groups) {
				t.Fatalf("routing modes did not round trip: %+v", loaded)
			}
			var version int
			if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 20 {
				t.Fatalf("migration did not publish v20: %d %v", version, err)
			}
			if _, err := d.db.Exec("UPDATE groups SET routing_mode='unknown'"); err == nil {
				t.Fatal("unknown mode escaped SQL constraint")
			}
		})
	}
}
