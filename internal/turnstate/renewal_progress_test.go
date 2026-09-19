package turnstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenewalLeadValidationAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  string
		seconds int
		valid   bool
	}{
		{"default", `{}`, 300, true},
		{"short automatic", `{"ttl_seconds":600}`, 150, true},
		{"minimum automatic", `{"ttl_seconds":60}`, 15, true},
		{"ten minutes", `{"renew_before_minutes":10}`, 600, true},
		{"twenty minutes", `{"renew_before_minutes":20}`, 1200, true},
		{"negative", `{"renew_before_minutes":-1}`, 0, false},
		{"over maximum", `{"renew_before_minutes":60}`, 0, false},
		{"matches lifetime", `{"ttl_seconds":600,"renew_before_minutes":10}`, 0, false},
		{"exceeds lifetime", `{"ttl_seconds":600,"renew_before_minutes":20}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			err := m.Update([]byte(tc.config))
			if (err == nil) != tc.valid {
				t.Fatalf("Update = %v, want valid %t", err, tc.valid)
			}
			if tc.valid && m.Status().RenewalLeadSeconds != tc.seconds {
				t.Fatalf("renewal lead = %d, want %d", m.Status().RenewalLeadSeconds, tc.seconds)
			}
		})
	}
}

func TestRenewalLeadReschedulesSuccessfulExitAcrossRestart(t *testing.T) {
	for _, pool := range []string{"direct", "static", "rotating"} {
		t.Run(pool, func(t *testing.T) {
			m, now := newTestManager(t)
			if pool != "direct" {
				field := "probe_proxies"
				if pool == "rotating" {
					field += "_rotating"
				}
				if err := m.Update([]byte(fmt.Sprintf(`{"%s":["http://dummy-user:dummy-password@proxy.invalid:8080"]}`, field))); err != nil {
					t.Fatal(err)
				}
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
				t.Fatalf("initial harvest = %+v, %v", result, err)
			}
			loaded := New()
			loaded.now, loaded.runProbe = m.now, m.runProbe
			if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
				t.Fatal(err)
			}
			if err := loaded.Update([]byte(`{"renew_before_minutes":20}`)); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(40*time.Minute - time.Second)
			if result, _ := loaded.Probe("", "", dummyCredential); result.Action != "fresh" {
				t.Fatalf("probe ran before configured lead: %+v", result)
			}
			*now = now.Add(time.Second)
			if result, err := loaded.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
				t.Fatalf("prior successful cooldown blocked earlier renewal: %+v, %v", result, err)
			}
		})
	}
}

func TestRenewalChangePreservesFailedAndLegacyCooldowns(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	failedKey := proxyKey("account-a", "model-a", "", false)
	m.state.Cooldowns[failedKey] = cooldown{Until: now.Add(55 * time.Minute)}
	m.state.Cooldowns["failed-rotating"] = cooldown{Until: now.Add(10 * time.Minute), Attempts: 10}
	m.state.Cooldowns[accountKey("account-a")] = cooldown{Until: now.Add(10 * time.Minute)}
	before := make(map[string]cooldown)
	for key, value := range m.state.Cooldowns {
		before[key] = value
	}
	if err := m.Update([]byte(`{"renew_before_minutes":20}`)); err != nil {
		t.Fatal(err)
	}
	for key, value := range before {
		if m.state.Cooldowns[key] != value {
			t.Fatalf("failure/legacy deadline changed: %q", key)
		}
	}
}

func TestRenewalChangeCanDelayRenewalAndReturnToAutomatic(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"renew_before_minutes":20}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	if _, err := m.Probe("", "", dummyCredential); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"renew_before_minutes":10}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(45 * time.Minute)
	if result, _ := m.Probe("", "", dummyCredential); result.Action != "fresh" {
		t.Fatalf("premature renewal: %+v", result)
	}
	if err := m.Update([]byte(`{"renew_before_minutes":0}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatalf("automatic renewal not restored: %+v %v", result, err)
	}
}

func TestProbeProgressTracksActualReservedCandidateAndRedactsCredentials(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://dummy-user:dummy-password@static.invalid:8080"],"probe_proxies_rotating":["http://dummy-user:dummy-password@rotating.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		close(started)
		<-finish
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	type outcome struct {
		result ProbeResult
		err    error
	}
	completed := make(chan outcome, 1)
	go func() { result, err := m.Probe("", "", dummyCredential); completed <- outcome{result, err} }()
	<-started
	progress := m.ProbeProgress()
	if !progress.Active || progress.Result.ProxyIndex != 1 || progress.Result.ProxyTotal != 2 || progress.Result.ProxyPool != "static" || progress.Result.ProxyAttempt != 1 || progress.Result.Exit != "http://***@static.invalid:8080" {
		t.Fatalf("incorrect active progress: %+v", progress)
	}
	encoded, _ := json.Marshal(progress)
	if strings.Contains(string(encoded), "dummy-password") || strings.Contains(string(encoded), "dummy-user") {
		t.Fatal("progress exposes proxy credentials")
	}
	close(finish)
	first := <-completed
	if first.err != nil || first.result.Action != "degraded" || first.result.ProxyIndex != 1 {
		t.Fatalf("first result: %+v", first)
	}
	if m.ProbeProgress().Active {
		t.Fatal("completed progress left active")
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	for attempt := 1; attempt <= 10; attempt++ {
		*now = now.Add(3 * time.Second)
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.ProxyIndex != 2 || result.ProxyTotal != 2 || result.ProxyPool != "rotating" || result.ProxyAttempt != attempt {
			t.Fatalf("rotating attempt %d: %+v, %v", attempt, result, err)
		}
	}
	*now = now.Add(11 * time.Minute)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{}, errors.New("dummy-password transport failure")
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.ProxyIndex != 2 || result.ProxyAttempt != 1 || result.Action != "error" || m.ProbeProgress().Active {
		t.Fatalf("retry/reset error progress: %+v, %v", result, err)
	}
}

func TestTemplateViewShowsRemainingLifetimeAndMaskedProvenance(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://dummy-user:dummy-password@proxy.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	issued := now.Add(-10 * time.Minute)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(issued)}, nil
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "harvested" {
		t.Fatalf("harvest: %+v, %v", result, err)
	}
	status := m.Status()
	row := status.Templates[0]
	if row.RemainingSeconds != 3000 || row.Source != "probe" || row.Exit != "http://***@proxy.invalid:8080" || !row.HarvestedAt.Equal(*now) || !status.ServerTime.Equal(*now) {
		t.Fatalf("incorrect template metadata: %+v", row)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), "dummy-password") || strings.Contains(string(raw), tokenAt(issued)) {
		t.Fatal("template table exposes credentials or token")
	}
	*now = now.Add(time.Minute)
	if m.Status().Templates[0].RemainingSeconds != 2940 {
		t.Fatal("remaining lifetime is not recalculated")
	}
	learn(t, m, tokenAt(*now))
	row = m.Status().Templates[0]
	if row.Source != "response" || row.Exit != "" || row.RemainingSeconds != 3600 {
		t.Fatalf("passive renewal retained old proxy provenance: %+v", row)
	}
}
