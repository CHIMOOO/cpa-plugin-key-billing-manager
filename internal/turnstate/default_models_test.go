package turnstate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultProbeModelsPreserveSavedSelections(t *testing.T) {
	defaults := []string{"gpt6", "gpt-5.6-sol"}
	for _, tc := range []struct {
		name  string
		saved string
		want  []string
	}{
		{name: "new installation", want: defaults},
		{name: "legacy config without model field", saved: `{"version":1,"config":{"dry_run":true}}`, want: defaults},
		{name: "saved operator list", saved: `{"version":1,"config":{"models":["operator-model","gpt6"]}}`, want: []string{"operator-model", "gpt6"}},
		{name: "saved empty list", saved: `{"version":1,"config":{"models":[]}}`, want: []string{}},
		{name: "saved null list", saved: `{"version":1,"config":{"models":null}}`, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billing.db")
			if tc.saved != "" {
				if err := os.WriteFile(path+".turn-state.json", []byte(tc.saved), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m := New()
			if models := m.Status().Config.Models; !reflect.DeepEqual(models, defaults) {
				t.Fatalf("unconfigured manager defaults = %v, want %v", models, defaults)
			}
			if err := m.Configure(path); err != nil {
				t.Fatal(err)
			}
			if models := m.Status().Config.Models; !reflect.DeepEqual(models, tc.want) {
				t.Fatalf("loaded models = %v, want %v", models, tc.want)
			}
			if tc.saved != "" {
				raw, err := os.ReadFile(path + ".turn-state.json")
				if err != nil || string(raw) != tc.saved {
					t.Fatalf("loading defaults rewrote the saved configuration: %s, %v", raw, err)
				}
			}
			// An unrelated save must preserve the operator's model list, including
			// a previously cleared list, across a process restart.
			if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
				t.Fatal(err)
			}
			loaded := New()
			if err := loaded.Configure(path); err != nil {
				t.Fatal(err)
			}
			if models := loaded.Status().Config.Models; !reflect.DeepEqual(models, tc.want) {
				t.Fatalf("restarted models = %v, want %v", models, tc.want)
			}
		})
	}
}

func TestClearingDefaultProbeModelsPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing.db")
	m := New()
	if err := m.Configure(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"models":[],"probe_accounts":["dummy-account"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(path); err != nil {
		t.Fatal(err)
	}
	loaded := New()
	if err := loaded.Configure(path); err != nil {
		t.Fatal(err)
	}
	if models := loaded.Status().Config.Models; len(models) != 0 {
		t.Fatalf("cleared model list was replaced with defaults: %v", models)
	}
	result, err := loaded.Probe("", "", func(string) (Credential, error) {
		t.Fatal("a cleared model list must not start a probe")
		return Credential{}, nil
	})
	if err != nil || result.Action != "error" || result.ReasonMessage.Key != "backend.turn_state_scope_required" {
		t.Fatalf("empty saved probe scope = %+v, %v", result, err)
	}
}
