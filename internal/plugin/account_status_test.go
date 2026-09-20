package plugin

import (
	"encoding/json"
	"errors"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestAccountStatusDescriptorsRequireSupportedExactIdentities(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	for _, test := range []struct {
		name string
		file hostAuthFile
		want bool
	}{
		{"file", hostAuthFile{ID: "codex.json", AuthIndex: "index-a", Name: "codex.json", Path: "/dummy/codex.json", Source: "file", Provider: "codex"}, true},
		{"disabled-file", hostAuthFile{ID: "codex.json", AuthIndex: "index-a", Name: "codex.json", Path: "/dummy/codex.json", Source: "file", Provider: "codex", Disabled: true}, true},
		{"virtual-child", hostAuthFile{ID: "project-a", AuthIndex: "index-a", Name: "source.json", Path: "/dummy/source.json", Source: "file", Provider: "gemini"}, false},
		{"unknown", hostAuthFile{ID: "dummy", AuthIndex: "index-a", Provider: "codex"}, false},
		{"native-codex", hostAuthFile{ID: "codex:apikey:abcdef123456", AuthIndex: "index-a", Provider: "codex", RuntimeOnly: true}, true},
		{"native-other-provider", hostAuthFile{ID: "openai-compatibility:dummy:abcdef123456", AuthIndex: "index-a", Provider: "openai-compatible-dummy", RuntimeOnly: true}, false},
		{"memory-not-config", hostAuthFile{ID: "dummy-memory", AuthIndex: "index-a", Provider: "codex", RuntimeOnly: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			patch, reason := a.accountStatusDescriptor(test.file, map[string]int{})
			if (patch != nil) != test.want || test.want && (patch.Name != test.file.ID || patch.AuthIndex != test.file.AuthIndex || reason != "") {
				t.Fatalf("patch=%+v reason=%q want_supported=%v", patch, reason, test.want)
			}
			if !test.want && reason == "" {
				t.Fatal("unsupported toggle has no reason")
			}
		})
	}
	physical := hostAuthFile{ID: "source.json", AuthIndex: "index-a", Name: "source.json", Path: "/dummy/source.json", Source: "file", Provider: "codex"}
	if patch, _ := a.accountStatusDescriptor(physical, map[string]int{"/dummy/source.json": 2}); patch != nil {
		t.Fatal("shared virtual source exposed an individual toggle")
	}
	a.hostSchema.Store(4)
	if patch, _ := a.accountStatusDescriptor(physical, nil); patch != nil {
		t.Fatal("unverified older host exposed status toggle")
	}
}

func TestAccountStatusRuntimeFallbackChecksIdentityOutsideRoutingLock(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	id := "codex:apikey:abcdef123456"
	ref := billing.CredentialFingerprint(id)
	a.observeCandidates([]SchedulerAuthCandidate{{ID: id, Provider: "codex", Attributes: map[string]string{"source": "config:codex[dummy]"}}})
	a.observeRouteCredential("request", id, "index-a")
	for _, test := range []struct {
		name string
		id   string
		err  bool
		want bool
	}{
		{"exact", id, false, true},
		{"different-id", "codex:apikey:abcdef654321", false, false},
		{"unavailable", id, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			a.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
				if method == hostAuthList {
					return mustMarshal(t, hostAuthListResponse{}), nil
				}
				if method != "host.auth.get_runtime" || payload.(map[string]string)["auth_index"] != "index-a" {
					t.Fatalf("unexpected host call %s", method)
				}
				if !a.routingMu.TryLock() {
					t.Fatal("host callback held routing lock")
				}
				a.routingMu.Unlock()
				if test.err {
					return nil, errors.New("dummy runtime unavailable")
				}
				return mustMarshal(t, map[string]any{"auth": hostAuthFile{ID: test.id, AuthIndex: "index-a", Provider: "codex", RuntimeOnly: true, Disabled: true}}), nil
			})
			var response struct {
				Accounts []struct {
					Ref      string              `json:"credential_ref"`
					Disabled bool                `json:"disabled"`
					Patch    *accountStatusPatch `json:"status_patch"`
				} `json:"accounts"`
			}
			if err := json.Unmarshal(a.getAccountRuntime(ManagementRequest{}).Body, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Accounts) != 1 || response.Accounts[0].Ref != ref || (response.Accounts[0].Patch != nil) != test.want {
				t.Fatalf("runtime fallback=%+v", response)
			}
			if test.want && !response.Accounts[0].Disabled {
				t.Fatal("stale inventory overrode fresh disabled state")
			}
		})
	}
}

func TestAccountStatusInventoryRejectsDuplicateIndices(t *testing.T) {
	files := []hostAuthFile{{ID: "a.json", AuthIndex: "duplicate"}, {ID: "b.json", AuthIndex: "duplicate"}}
	index := accountStatusIdentities(files)
	for _, file := range files {
		if !index.ambiguous[billing.CredentialFingerprint(file.ID)] {
			t.Fatal("ambiguous index was not rejected")
		}
	}
}
