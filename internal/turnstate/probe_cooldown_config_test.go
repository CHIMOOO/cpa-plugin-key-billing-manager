package turnstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProbeCooldownDefaultsAndLegacyConfiguration(t *testing.T) {
	defaults := DefaultConfig()
	check := func(t *testing.T, cfg Config, want [4]int) {
		t.Helper()
		got := [4]int{cfg.ProbeStaticCooldownMinutes, cfg.ProbeRotatingCooldownMinutes, cfg.ProbeAccountCooldownMinutes, cfg.ProbeRotatingMaxAttempts}
		if got != want {
			t.Fatalf("cooldown settings = %v, want %v", got, want)
		}
	}
	wantDefault := [4]int{55, 10, 10, 10}
	check(t, defaults, wantDefault)
	m, _ := newTestManager(t)
	// Real legacy files omit the new fields, rather than writing explicit zero.
	raw, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(legacy["config"], &cfg); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"probe_static_cooldown_minutes", "probe_rotating_cooldown_minutes", "probe_account_cooldown_minutes", "probe_rotating_max_attempts"} {
		delete(cfg, field)
	}
	legacy["config"], _ = json.Marshal(cfg)
	raw, _ = json.Marshal(legacy)
	if err := os.WriteFile(m.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	check(t, m.Status().Config, wantDefault)
	if err := m.CommitConfigUpload(stageTestConfig(t, m, []byte(`{"inject_mode":"always"}`))); err != nil {
		t.Fatal(err)
	}
	check(t, m.Status().Config, wantDefault)
	if err := m.Update([]byte(`{"probe_static_cooldown_minutes":7,"probe_rotating_cooldown_minutes":4,"probe_account_cooldown_minutes":3,"probe_rotating_max_attempts":6}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitConfigUpload(stageTestConfig(t, m, []byte(`{"dry_run":true}`))); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	check(t, m.Status().Config, [4]int{7, 4, 3, 6})
}

func TestProbeCooldownConfigurationRejectsInvalidBoundsAtomically(t *testing.T) {
	m, _ := newTestManager(t)
	for _, field := range []string{"probe_static_cooldown_minutes", "probe_rotating_cooldown_minutes", "probe_account_cooldown_minutes", "probe_rotating_max_attempts"} {
		maximum := 1440
		if field == "probe_rotating_max_attempts" {
			maximum = 100
		}
		for _, value := range []string{"-1", fmt.Sprint(maximum + 1), "1.5", `"5"`} {
			before := m.Status().Config
			payload := []byte(fmt.Sprintf(`{"%s":%s}`, field, value))
			if err := m.Update(payload); err == nil {
				t.Fatalf("accepted invalid %s = %s", field, value)
			}
			if !reflect.DeepEqual(before, m.Status().Config) {
				t.Fatalf("invalid %s changed configuration", field)
			}
		}
		for _, value := range []int{0, 1, maximum} {
			if err := m.Update([]byte(fmt.Sprintf(`{"%s":%d}`, field, value))); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCustomStaticCooldownAndOrdinarySpacing(t *testing.T) {
	for _, proxy := range []string{"", "http://static.invalid:8000"} {
		t.Run(proxy, func(t *testing.T) {
			m, now := newTestManager(t)
			raw, _ := json.Marshal(map[string]any{"probe_static_cooldown_minutes": 7, "probe_account_cooldown_minutes": 4, "probe_proxies": []string{proxy}})
			if err := m.Update(raw); err != nil {
				t.Fatal(err)
			}
			m.runProbe = degradedProbe
			started := *now
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.Action != "degraded" || result.ProxyAttemptLimit != 0 || !result.NextCheckAt.Equal(started.Add(2*time.Second)) {
				t.Fatal("ordinary attempt spacing changed", result, err)
			}
			if got := m.state.Cooldowns[accountKey("account-a")].Until; !got.Equal(started.Add(2 * time.Second)) {
				t.Fatal("ordinary degradation used configured account refusal cooldown", got)
			}
			if got := m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, false)].Until; !got.Equal(started.Add(7 * time.Minute)) {
				t.Fatal("static cooldown ignored setting", got)
			}
			m = readRestarted(t, m)
			m.runProbe = degradedProbe
			*now = started.Add(7*time.Minute - time.Nanosecond)
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
				t.Fatal("static exit retried early", result, err)
			}
			*now = started.Add(7 * time.Minute)
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" {
				t.Fatal("static exit failed to resume", result, err)
			}
		})
	}
}

func TestCustomRotatingCooldownLimitSurvivesRestartAndConfigurationEdits(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:8000","http://second.invalid:8000"],"probe_rotating_cooldown_minutes":7,"probe_rotating_max_attempts":3}`)); err != nil {
		t.Fatal(err)
	}
	started := *now
	for attempt := 1; attempt <= 3; attempt++ {
		m.runProbe = degradedProbe
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" || result.ProxyAttempt != attempt || result.ProxyAttemptLimit != 3 || result.ProxyIndex != (attempt-1)%2+1 {
			t.Fatalf("attempt %d did not share rotating budget across URLs: %+v %v", attempt, result, err)
		}
		m = readRestarted(t, m)
		*now = now.Add(3 * time.Second)
	}
	id := rotatingBudgetKey("account-a", "model-a")
	before := m.state.Cooldowns[id]
	if before.Attempts != 3 || !before.Until.Equal(started.Add(7*time.Minute)) {
		t.Fatal("custom rotating reservation was not durable", before)
	}
	if err := m.Update([]byte(`{"probe_rotating_cooldown_minutes":1,"probe_rotating_max_attempts":2}`)); err != nil {
		t.Fatal(err)
	}
	if m.state.Cooldowns[id] != before {
		t.Fatal("settings edit rewrote an existing cooldown or its attempts")
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" || !result.NextCheckAt.Equal(before.Until) {
		t.Fatal("settings edit bypassed retained cooldown", result, err)
	}
	// Raising the cap admits only additional attempts in the existing window.
	if err := m.Update([]byte(`{"probe_rotating_max_attempts":4}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = degradedProbe
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyAttempt != 4 || result.ProxyAttemptLimit != 4 {
		t.Fatal("raised limit reset prior attempts", result, err)
	}
	if got := m.state.Cooldowns[id]; got.Attempts != 4 || !got.Until.Equal(before.Until) {
		t.Fatal("raised limit changed active window", got)
	}
	*now = before.Until
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.ProxyAttempt != 1 || result.Action != "degraded" {
		t.Fatal("rotating window did not restart", result, err)
	}
	if got := m.state.Cooldowns[id]; !got.Until.Equal(now.Add(time.Minute)) {
		t.Fatal("new rotating window did not use saved duration", got)
	}
}

func TestCustomAccountCooldownAppliesToOAuthAndUpstreamRefusals(t *testing.T) {
	for _, status := range []int{0, 401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"models":["model-a","model-b"],"probe_account_cooldown_minutes":3,"probe_proxies":["http://first.invalid:8000","http://second.invalid:8000"]}`)); err != nil {
				t.Fatal(err)
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return ProbeResponse{Status: status}, nil }
			fetch := dummyCredential
			if status == 0 {
				fetch = func(string) (Credential, error) { return Credential{}, errors.New("dummy credential unavailable") }
			}
			started := *now
			result, err := m.Probe("account-a", "model-a", fetch)
			if err != nil || result.Action != "error" || !strings.Contains(result.Reason, "3 minutes") {
				t.Fatal("account refusal did not report configured cooldown", result, err)
			}
			parameter := "v0"
			if status == 401 || status == 403 {
				parameter = "v1"
			}
			if result.ReasonMessage.Params[parameter] != "3" {
				t.Fatal("account pause metadata lost duration", result.ReasonMessage)
			}
			before := m.state.Cooldowns[accountKey("account-a")]
			if !before.Until.Equal(started.Add(3 * time.Minute)) {
				t.Fatal("incorrect account pause", before)
			}
			if err := m.Update([]byte(`{"probe_account_cooldown_minutes":1}`)); err != nil {
				t.Fatal(err)
			}
			m = readRestarted(t, m)
			if m.state.Cooldowns[accountKey("account-a")] != before {
				t.Fatal("settings edit or restart rewrote existing pause")
			}
			*now = started.Add(2 * time.Minute)
			if result, err := m.Probe("account-a", "model-b", dummyCredential); err != nil || result.Action != "account_wait" {
				t.Fatal("account pause bypassed by model switch", result, err)
			}
			*now = before.Until
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			if result, err := m.Probe("account-a", "model-b", dummyCredential); err != nil || result.Action != "harvested" {
				t.Fatal("account did not resume", result, err)
			}
		})
	}
}

func TestCustomRotatingLimitPreservesLegacyCountsAboveTen(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:8000","http://second.invalid:8000"],"probe_rotating_max_attempts":20}`)); err != nil {
		t.Fatal(err)
	}
	until := now.Add(8 * time.Minute)
	for _, proxy := range m.state.Config.ProbeProxiesRotating {
		// Before shared budgets existed, each URL kept its own attempts.
		m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, true)] = cooldown{Until: until, Attempts: 8}
	}
	m.runProbe = degradedProbe
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "degraded" || result.ProxyAttempt != 17 || result.ProxyAttemptLimit != 20 {
		t.Fatal("legacy attempt counts were truncated to the old default", result, err)
	}
	m = readRestarted(t, m)
	budget := m.state.Cooldowns[rotatingBudgetKey("account-a", "model-a")]
	if budget.Attempts != 17 || !budget.Until.Equal(until) {
		t.Fatal("legacy counts or their deadline were lost during migration", budget)
	}
	if err := m.Update([]byte(`{"probe_rotating_max_attempts":12}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("lowering the limit lost known attempts", result, err)
	}
}

func TestCooldownEditsPreserveFreshnessAndHourlyBudget(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_hourly_limit":1,"probe_static_cooldown_minutes":1440,"probe_rotating_cooldown_minutes":1440,"probe_account_cooldown_minutes":1440}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatal(result, err)
	}
	until := m.state.Cooldowns[proxyKey("account-a", "model-a", "", false)].Until
	if !until.Equal(now.Add(55 * time.Minute)) {
		t.Fatal("failure cooldown delayed a successful template renewal", until)
	}
	if err := m.Update([]byte(`{"probe_static_cooldown_minutes":1,"probe_rotating_cooldown_minutes":1,"probe_account_cooldown_minutes":1,"probe_rotating_max_attempts":100}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClearCooldowns("", ""); err != nil {
		t.Fatal(err)
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "fresh" {
		t.Fatal("settings edit reprobed a fresh template", result, err)
	}
	*now = until
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "budget_wait" {
		t.Fatal("settings edit reset hourly budget", result, err)
	}
}
