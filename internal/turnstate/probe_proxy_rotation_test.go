package turnstate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var rotationProxies = []string{
	"socks5://dummy-session-a:dummy-password@proxy.invalid:3000",
	"socks5://dummy-session-b:dummy-password@proxy.invalid:3000",
	"socks5://dummy-session-c:dummy-password@proxy.invalid:3000",
}

func configureRotationProxies(t *testing.T, m *Manager, statics, rotating []string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"probe_proxies": statics, "probe_proxies_rotating": rotating})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Update(raw); err != nil {
		t.Fatal(err)
	}
}

func TestProxyCursorContinuesAcrossSuccessfulBucketsRestartsAndRenewals(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"ttl_seconds":60,"probe_accounts":["account-a","account-b"],"models":["model-a","model-b"]}`)); err != nil {
		t.Fatal(err)
	}
	configureRotationProxies(t, m, rotationProxies[:2], rotationProxies[2:])
	for call := 0; call < 8; call++ {
		if call == 4 {
			*now = now.Add(45 * time.Second)
		}
		m = readRestarted(t, m)
		m.runProbe = func(_ Credential, _, proxy string) (ProbeResponse, error) {
			if proxy != rotationProxies[call%3] {
				t.Errorf("probe %d restarted the exit pool: %q", call, proxy)
			}
			return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
		}
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "harvested" || result.ProxyIndex != call%3+1 || result.ProxyTotal != 3 {
			t.Fatalf("success %d: %+v, %v", call, result, err)
		}
		if (result.ProxyPool == "rotating") != (call%3 == 2) {
			t.Fatalf("wrong pool for shared cursor: %+v", result)
		}
		*now = now.Add(3 * time.Second)
	}
}

func TestProxyCursorKeepsNextSurvivingEntryAfterPoolEdits(t *testing.T) {
	for _, scenario := range []struct {
		name string
		pool []string
		want string
	}{
		{"reorder", []string{rotationProxies[2], rotationProxies[0], rotationProxies[1]}, rotationProxies[1]},
		{"delete-previous", rotationProxies[1:], rotationProxies[1]},
		{"delete-next", []string{rotationProxies[0], rotationProxies[2]}, rotationProxies[2]},
		{"replace-all", []string{"http://new.invalid:8080"}, "http://new.invalid:8080"},
		{"clear-all", []string{}, ""},
		{"insert-before-next", append([]string{"http://new.invalid:8080"}, rotationProxies...), rotationProxies[1]},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			m, now := newTestManager(t)
			configureRotationProxies(t, m, rotationProxies, nil)
			m.runProbe = degradedProbe
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyIndex != 1 {
				t.Fatal(result, err)
			}
			configureRotationProxies(t, m, scenario.pool, nil)
			m = readRestarted(t, m)
			m.runProbe = func(_ Credential, _, proxy string) (ProbeResponse, error) {
				if proxy != scenario.want {
					t.Errorf("edited pool restarted or skipped next surviving entry: got %q, want %q", proxy, scenario.want)
				}
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			*now = now.Add(3 * time.Second)
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
				t.Fatal(result, err)
			}
		})
	}
}

func TestProxyCursorPreservesOrderDuringAutomaticRemoval(t *testing.T) {
	for _, rotating := range []bool{false, true} {
		m, now := newTestManager(t)
		if rotating {
			configureRotationProxies(t, m, nil, rotationProxies)
		} else {
			configureRotationProxies(t, m, rotationProxies, nil)
		}
		if err := m.Update([]byte(`{"probe_drop_degraded_proxies":true,"probe_min_proxies":1}`)); err != nil {
			t.Fatal(err)
		}
		m.runProbe = degradedProbe
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyDisposition != "removed" || result.ProxyRemaining != 2 {
			t.Fatal(result, err)
		}
		m = readRestarted(t, m)
		m.runProbe = func(_ Credential, _, proxy string) (ProbeResponse, error) {
			if proxy != rotationProxies[1] {
				t.Errorf("removal skipped the next URL (rotating=%v): %q", rotating, proxy)
			}
			return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
		}
		*now = now.Add(3 * time.Second)
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" || result.ProxyIndex != 1 || result.ProxyTotal != 2 {
			t.Fatal(result, err)
		}
	}
}

func TestProxyCursorSkipsCoolingExitAndWraps(t *testing.T) {
	m, now := newTestManager(t)
	configureRotationProxies(t, m, rotationProxies, nil)
	m.runProbe = degradedProbe
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyIndex != 1 {
		t.Fatal(result, err)
	}
	m.state.Cooldowns[proxyKey("account-a", "model-a", rotationProxies[1], false)] = cooldown{Until: now.Add(time.Hour)}
	*now = now.Add(3 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyIndex != 3 {
		t.Fatal("cursor did not skip cooling exit", result, err)
	}
	*now = now.Add(56 * time.Minute)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyIndex != 1 {
		t.Fatal("cursor did not wrap to the next eligible exit", result, err)
	}
}

func TestProxyCursorIsAtomicAndReservationSurvivesFailedResultWrite(t *testing.T) {
	for _, failedWrite := range []int{1, 2} {
		m, now := newTestManager(t)
		configureRotationProxies(t, m, rotationProxies, nil)
		writes, requests := 0, 0
		m.writeState = func(path string, raw []byte) error {
			writes++
			if strings.Contains(string(raw), "dummy-password") {
				t.Error("runtime cursor persisted proxy credentials")
			}
			if writes == failedWrite {
				return errors.New("simulated write failure")
			}
			return atomicWriteState(path, raw)
		}
		m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
			requests++
			return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
		}
		if _, err := m.Probe("", "", dummyCredential); err == nil || requests != failedWrite-1 {
			t.Fatalf("write %d did not atomically reserve the attempted URL: requests=%d err=%v", failedWrite, requests, err)
		}
		m = readRestarted(t, m)
		m.runProbe = degradedProbe
		*now = now.Add(3 * time.Second)
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyIndex != failedWrite {
			t.Fatalf("write %d advanced an unreserved attempt or repeated a reserved one: %+v, %v", failedWrite, result, err)
		}
	}
}
