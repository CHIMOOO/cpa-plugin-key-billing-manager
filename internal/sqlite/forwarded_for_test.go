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

func TestForwardedForBlockDefaultsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forwarded.db")
	d := openDatabase(t, path)
	var (
		enabled          bool
		keywords, stored string
	)
	if err := d.db.QueryRow("SELECT enabled, model_keywords_json, message FROM forwarded_for_block WHERE id = 1").Scan(&enabled, &keywords, &stored); err != nil {
		t.Fatal(err)
	}
	if enabled || keywords != "[]" || stored != "" {
		t.Fatalf("fresh row = %v %q %q", enabled, keywords, stored)
	}
	state := mustLoad(t, d).State
	if !reflect.DeepEqual(state.ForwardedForBlock, billing.DefaultForwardedForBlock()) {
		t.Fatalf("fresh settings = %+v", state.ForwardedForBlock)
	}

	state.ForwardedForBlock = billing.ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt", "Claude"}, Message: "自定义提示"}
	mustSave(t, d, state, billing.Changes{ForwardedForBlock: true})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = openDatabase(t, path)
	loaded := mustLoad(t, d).State
	if !reflect.DeepEqual(loaded.ForwardedForBlock, state.ForwardedForBlock) {
		t.Fatalf("settings not persisted: %+v", loaded.ForwardedForBlock)
	}

	// A save that does not name the setting leaves the stored row alone.
	loaded.ForwardedForBlock = billing.DefaultForwardedForBlock()
	mustSave(t, d, loaded, billing.Changes{AccessControl: true})
	if after := mustLoad(t, d).State; !reflect.DeepEqual(after.ForwardedForBlock, state.ForwardedForBlock) {
		t.Fatalf("unrelated save rewrote the setting: %+v", after.ForwardedForBlock)
	}

	// Stored values are normalized on the way in; an empty message is the default.
	if _, err := d.db.Exec(`UPDATE forwarded_for_block SET enabled = 1, model_keywords_json = '[" gpt ","GPT","",null]', message = '' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	want := billing.ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}, Message: billing.DefaultForwardedForBlockMessage}
	if got := mustLoad(t, d).State.ForwardedForBlock; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized load = %+v, want %+v", got, want)
	}

	for _, corrupt := range []string{
		`UPDATE forwarded_for_block SET enabled = 1, model_keywords_json = '[]' WHERE id = 1`,
		`UPDATE forwarded_for_block SET enabled = 0, model_keywords_json = '{' WHERE id = 1`,
	} {
		if _, err := d.db.Exec(corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Load(time.Time{}, time.Time{}); err == nil {
			t.Fatalf("corrupt settings accepted: %s", corrupt)
		}
	}
}

func TestForwardedForBlockSaveIsAtomic(t *testing.T) {
	d := openTestDB(t)
	state := billing.NewState()
	state.AccessControl = billing.AccessControl{Enabled: true, DenyUngrouped: true}
	mustSave(t, d, state, billing.Changes{AccessControl: true, ForwardedForBlock: true})
	before := mustLoad(t, d).State
	if _, err := d.db.Exec("CREATE TRIGGER fail_forwarded_write BEFORE UPDATE ON forwarded_for_block BEGIN SELECT RAISE(ABORT, 'dummy failure'); END"); err != nil {
		t.Fatal(err)
	}
	state.AccessControl = billing.AccessControl{}
	state.ForwardedForBlock = billing.ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}, Message: "blocked"}
	if err := d.Save(state, billing.Changes{AccessControl: true, ForwardedForBlock: true}); err == nil {
		t.Fatal("expected transactional failure")
	}
	if after := mustLoad(t, d).State; !reflect.DeepEqual(before, after) {
		t.Fatalf("failed save committed a partial change: %+v", after)
	}
}

func TestV15ForwardedForBlockMigrationPreservesDataAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "migrate", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v15.db")
			raw, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			oldSchema := strings.TrimSuffix(schema, forwardedForBlockSchema)
			if oldSchema == schema || strings.Contains(oldSchema, "forwarded_for_block") {
				t.Fatal("v15 fixture still contains the v16 table")
			}
			if _, err := raw.Exec(oldSchema + `
PRAGMA user_version=15;
UPDATE access_control SET enabled = 0, deny_ungrouped = 1 WHERE id = 1;
INSERT INTO api_keys(scope,preview,label,route_bindings_json) VALUES('scope','dum…001','History','{"route_ids":["legacy"]}');
INSERT INTO routes(position,id,name,rule_json) VALUES(0,'legacy','Legacy','{"models":["model"]}');
INSERT INTO groups(position,id,name,route_ids_json) VALUES(0,'team','Team','["legacy"]');
INSERT INTO key_groups(scope,position,group_id) VALUES('scope',0,'team');
INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'scope',0,0),(2,2,'scope',1,0),(3,3,'scope',0,2.5);
INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'preserved');
`); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := raw.Exec("CREATE TABLE forwarded_for_block(marker TEXT); INSERT INTO forwarded_for_block VALUES('preserve')"); err != nil {
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
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 15 {
					t.Fatalf("migration version did not roll back: %d %v", version, err)
				}
				var marker string
				if err := raw.QueryRow("SELECT marker FROM forwarded_for_block").Scan(&marker); err != nil || marker != "preserve" {
					t.Fatal("existing table lost", err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM groups").Scan(&groups); err != nil || groups != 1 {
					t.Fatal("existing data lost", groups, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var version int
			if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion || schemaVersion != 16 {
				t.Fatalf("schema version = %d (%d), err = %v", version, schemaVersion, err)
			}
			state := mustLoad(t, d).State
			if !reflect.DeepEqual(state.ForwardedForBlock, billing.DefaultForwardedForBlock()) {
				t.Fatalf("migration did not add the disabled default: %+v", state.ForwardedForBlock)
			}
			if state.AccessControl != (billing.AccessControl{DenyUngrouped: true}) || len(state.Groups) != 1 ||
				!reflect.DeepEqual(state.Keys["scope"].GroupIDs, []string{"team"}) || state.Keys["scope"].Label != "History" ||
				len(state.Keys["scope"].RouteBindings.RouteIDs) != 1 || len(state.Routes) != 1 {
				t.Fatalf("migration changed existing configuration: %+v", state)
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
