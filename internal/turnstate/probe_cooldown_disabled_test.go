package turnstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const disabledProbeCooldowns = `{"probe_static_cooldown_minutes":0,"probe_rotating_cooldown_minutes":0,"probe_account_cooldown_minutes":0,"probe_rotating_max_attempts":0}`

func TestDisabledCooldownsKeepRotatingStaticExitsAndPreservePacing(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://first.invalid:8000","http://second.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	until := now.Add(55 * time.Minute)
	// Pre-upgrade account records have no separate pacing timestamp.
	m.state.Cooldowns[accountKey("account-a")] = cooldown{Until: until}
	for _, proxy := range m.state.Config.ProbeProxies {
		m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, false)] = cooldown{Until: until}
	}
	if err := m.Update([]byte(disabledProbeCooldowns)); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 6; attempt++ {
		m.runProbe = degradedProbe
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" || result.ProxyIndex != attempt%2+1 {
			t.Fatal("disabled gates did not continue rotating IPs", result, err)
		}
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "account_wait" || !result.NextCheckAt.Equal(now.Add(2*time.Second)) {
			t.Fatal("disabling account cooldown removed minimum pacing", result, err)
		}
		m = readRestarted(t, m)
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "account_wait" {
			t.Fatal("restart discarded minimum pacing", result, err)
		}
		*now = now.Add(2 * time.Second)
	}
	if budget := m.Status().ProbeBudget; budget.Used != 6 {
		t.Fatal("unlimited retries lost hourly usage", budget)
	}
	for _, proxy := range m.state.Config.ProbeProxies {
		if got := m.state.Cooldowns[proxyKey("account-a", "model-a", proxy, false)].Until; !got.Equal(until) {
			t.Fatal("disabled static policy destroyed existing deadline", got)
		}
	}
	if err := m.Update([]byte(`{"probe_static_cooldown_minutes":55}`)); err != nil {
		t.Fatal(err)
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("reenabling static cooldown lost old reservations", result, err)
	}
}

func TestUnlimitedRotatingRetriesKeepActiveWindowAndExplicitZeroLimit(t *testing.T) {
	for _, settings := range []string{
		`{"probe_rotating_cooldown_minutes":0,"probe_rotating_max_attempts":2}`,
		`{"probe_rotating_cooldown_minutes":7,"probe_rotating_max_attempts":0}`,
		`{"probe_rotating_cooldown_minutes":0,"probe_rotating_max_attempts":0}`,
	} {
		t.Run(settings, func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:8000","http://second.invalid:8000"]}`)); err != nil {
				t.Fatal(err)
			}
			id := rotatingBudgetKey("account-a", "model-a")
			until := now.Add(5 * time.Minute)
			m.state.Cooldowns[id] = cooldown{Until: until, Attempts: 10}
			if err := m.Update([]byte(settings)); err != nil {
				t.Fatal(err)
			}
			for attempt := 11; attempt <= 13; attempt++ {
				m.runProbe = degradedProbe
				result, err := m.Probe("", "", dummyCredential)
				if err != nil || result.Action != "degraded" || result.ProxyAttempt != attempt || result.ProxyAttemptLimit != 0 {
					t.Fatal("unlimited retries still stopped at old cap", result, err)
				}
				raw, err := json.Marshal(result)
				if err != nil || !strings.Contains(string(raw), `"proxy_attempt_limit":0`) {
					t.Fatal("explicit unlimited limit omitted from management result", string(raw), err)
				}
				m = readRestarted(t, m)
				*now = now.Add(2 * time.Second)
			}
			if got := m.state.Cooldowns[id]; got.Attempts != 13 || !got.Until.Equal(until) {
				t.Fatal("unlimited policy changed active retry accounting", got)
			}
			if err := m.Update([]byte(`{"probe_rotating_cooldown_minutes":7,"probe_rotating_max_attempts":10}`)); err != nil {
				t.Fatal(err)
			}
			if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" || !result.NextCheckAt.Equal(until) {
				t.Fatal("reenabling limits reset known window counts", result, err)
			}
		})
	}
}

func TestDisabledRotatingWindowCreatesNoHiddenDeadline(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:8000","http://second.invalid:8000"],"probe_rotating_cooldown_minutes":0,"probe_rotating_max_attempts":1}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = degradedProbe
	for attempt := 0; attempt < 12; attempt++ {
		result, err := m.Probe("", "", dummyCredential)
		if err != nil || result.Action != "degraded" || result.ProxyAttemptLimit != 0 || result.ProxyIndex != attempt%2+1 {
			t.Fatal("disabled rotating window stopped progress", result, err)
		}
		if _, exists := m.state.Cooldowns[rotatingBudgetKey("account-a", "model-a")]; exists {
			t.Fatal("disabled rotating window manufactured a hidden wait")
		}
		*now = now.Add(2 * time.Second)
	}
	m = readRestarted(t, m)
	if err := m.Update([]byte(`{"probe_rotating_cooldown_minutes":7}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = degradedProbe
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" || result.ProxyAttempt != 1 || result.ProxyAttemptLimit != 1 {
		t.Fatal("reenabling created an invented historical pause", result, err)
	}
	*now = now.Add(2 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("reenabled limit did not resume enforcement", result, err)
	}
}

func TestDisabledAccountCooldownRetainsPacingForCredentialAndHTTPFailures(t *testing.T) {
	for _, status := range []int{0, 401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, now := newTestManager(t)
			if err := m.Update([]byte(disabledProbeCooldowns)); err != nil {
				t.Fatal(err)
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return ProbeResponse{Status: status}, nil }
			fetch := dummyCredential
			wantKey := "backend.turn_state_account_refused_no_cooldown"
			if status == 0 {
				fetch = func(string) (Credential, error) { return Credential{}, errors.New("dummy credential unavailable") }
				wantKey = "backend.turn_state_credential_invalid_no_cooldown"
			} else if status == 429 {
				wantKey = "backend.turn_state_rate_limited_no_cooldown"
			}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := m.Probe("", "", fetch)
				if err != nil || result.Action != "error" || result.ReasonMessage.Key != wantKey || strings.Contains(result.Reason, "paused") {
					t.Fatal("disabled account pause reported an active pause", result, err)
				}
				pause := m.state.Cooldowns[accountKey("account-a")]
				if !pause.Until.Equal(now.Add(2*time.Second)) || !pause.PacingUntil.Equal(pause.Until) {
					t.Fatal("disabled account failure did not use minimum pacing", pause)
				}
				if result, err := m.Probe("", "", fetch); err != nil || result.Action != "account_wait" {
					t.Fatal("disabled account failure allowed immediate busy-loop", result, err)
				}
				*now = now.Add(2 * time.Second)
			}
		})
	}
}

func TestDisabledCooldownsPreserveRenewalsAndHourlyLimits(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(disabledProbeCooldowns)); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"probe_hourly_limit":1}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatal(result, err)
	}
	*now = now.Add(time.Minute)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "fresh" {
		t.Fatal("disabled failure cooldowns defeated freshness", result, err)
	}
	// Even an orphaned successful reservation retains its renewal deadline.
	delete(m.state.Templates, key("account-a", "model-a"))
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("disabled failure cooldown bypassed successful renewal marker", result, err)
	}
	*now = now.Add(54 * time.Minute)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "budget_wait" {
		t.Fatal("disabled cooldown bypassed hourly cap", result, err)
	}
}

func TestUnlimitedRotatingCounterSaturates(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies_rotating":["http://first.invalid:8000"],"probe_rotating_max_attempts":0}`)); err != nil {
		t.Fatal(err)
	}
	id := rotatingBudgetKey("account-a", "model-a")
	m.state.Cooldowns[id] = cooldown{Until: now.Add(time.Minute), Attempts: int(^uint(0) >> 1)}
	m.runProbe = degradedProbe
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.ProxyAttempt != maxProbeUsageCount || m.state.Cooldowns[id].Attempts != maxProbeUsageCount {
		t.Fatal("unlimited retry count overflowed", result, err)
	}
	if err := m.Update([]byte(`{"probe_rotating_max_attempts":100}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "cooling" {
		t.Fatal("overflowing old counter disabled restored limit", result, err)
	}
}
