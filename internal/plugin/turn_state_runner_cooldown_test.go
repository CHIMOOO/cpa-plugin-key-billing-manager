package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/turnstate"
)

func clearRunnerCooldowns(t *testing.T, a *App) {
	t.Helper()
	response := a.clearTurnStateCooldowns(ManagementRequest{Body: []byte(`{"confirm":true}`)})
	if response.StatusCode != http.StatusOK || response.Headers.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("clear cooldown response=%d %s", response.StatusCode, response.Body)
	}
}

func TestServerRunnerCooldownClearRechecksNextHeartbeat(t *testing.T) {
	a, now := runnerFixture(t)
	runnerEnable(t, a, true)
	calls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		calls++
		return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: "cooldown", NextCheckAt: now.Add(time.Minute)})
	}
	before := runnerState(t, runnerTick(a, "instance-a"))
	*now = now.Add(5 * time.Second)
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 1 {
		t.Fatal("cached cooldown did not suppress intermediate probes")
	}
	for _, body := range []string{`{"confirm":false}`, `{"confirm":true,"account":"dummy-account"}`} {
		response := a.clearTurnStateCooldowns(ManagementRequest{Body: []byte(body)})
		if response.StatusCode != http.StatusBadRequest || !a.turnStateRunner.status().NextCheckAt.Equal(before.NextCheckAt) {
			t.Fatal("rejected clear changed the runner schedule", string(response.Body))
		}
	}
	clearRunnerCooldowns(t, a)
	after := a.turnStateRunner.status()
	if calls != 1 || !after.Enabled || after.Revision != before.Revision || !after.NextCheckAt.IsZero() || after.NextTickSeconds > 5 {
		t.Fatal("clear must invalidate only scheduling, without probing or changing collection intent", after)
	}
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 2 {
		t.Fatal("next heartbeat retained the pre-clear cooldown hint")
	}
}

func TestServerRunnerCooldownClearSupersedesInflightHintAndKeepsStop(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "enabled"
		if stop {
			name = "stopped"
		}
		t.Run(name, func(t *testing.T) {
			a, now := runnerFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			a.turnStateRunner.probe = func() ManagementResponse {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: "cooldown", NextCheckAt: now.Add(time.Minute)})
			}
			runnerEnable(t, a, true)
			done := make(chan ManagementResponse, 1)
			go func() { done <- runnerTick(a, "instance-a") }()
			<-entered
			if stop {
				runnerEnable(t, a, false)
			}
			clearRunnerCooldowns(t, a)
			if response := runnerTick(a, "instance-a"); response.StatusCode != http.StatusConflict || calls.Load() != 1 {
				t.Error("clear admitted another probe before the in-flight request finished")
			}
			close(release)
			status := runnerState(t, <-done)
			if status.Enabled == stop || status.InFlight || !status.NextCheckAt.IsZero() {
				t.Fatal("in-flight completion restored a stale due hint or changed collection intent", status)
			}
			if len(status.Events) != 1 || !status.Events[0].Result.NextCheckAt.Equal(status.NextCheckAt) {
				t.Fatal("event retained the scheduling hint invalidated by cooldown clearing", status)
			}
			runnerState(t, runnerTick(a, "instance-a"))
			wantCalls := int32(2)
			if stop {
				wantCalls = 1
				clearRunnerCooldowns(t, a)
				runnerState(t, runnerTick(a, "instance-a"))
			}
			if calls.Load() != wantCalls || a.turnStateRunner.status().Enabled == stop {
				t.Fatal("post-clear heartbeat did not respect the operator's collection intent")
			}
		})
	}
}

func TestServerRunnerEventsReportEffectiveSchedulerDeadline(t *testing.T) {
	for _, test := range []struct {
		name   string
		action string
		hint   time.Duration
		wait   time.Duration
	}{
		{"account-pause", "account_wait", 10 * time.Minute, time.Minute},
		{"legacy-account-pause", "account_wait", 55 * time.Minute, time.Minute},
		{"refusal-allows-other-accounts", "error", 2 * time.Second, 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, now := runnerFixture(t)
			runnerEnable(t, a, true)
			a.turnStateRunner.probe = func() ManagementResponse {
				return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: test.action, NextCheckAt: now.Add(test.hint)})
			}
			status := runnerState(t, runnerTick(a, "instance-a"))
			if !status.NextCheckAt.Equal(now.Add(test.wait)) || len(status.Events) != 1 || !status.Events[0].Result.NextCheckAt.Equal(status.NextCheckAt) {
				t.Fatal("event did not reflect the effective scheduler check deadline", status)
			}
		})
	}
}

func TestServerRunnerCooldownClearPreservesFreshTemplates(t *testing.T) {
	a, _ := runnerFixture(t)
	if err := a.turnState.Update([]byte(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	a.turnState.Before("dummy-learn", "dummy-account", "dummy-model", nil)
	if err := a.turnState.Learn("dummy-learn", "dummy-account", "dummy-model", http.Header{turnstate.Header: {rpcTurnStateTemplate()}}); err != nil {
		t.Fatal(err)
	}
	before := a.turnState.Status().Templates
	if len(before) != 1 {
		t.Fatal("fixture did not learn a fresh template")
	}
	planningCalls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		planningCalls++
		result, err := a.turnState.Probe("", "", func(string) (turnstate.Credential, error) {
			t.Fatal("clearing cooldowns unnecessarily re-probed a fresh template")
			return turnstate.Credential{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return JSONResponse(http.StatusOK, result)
	}
	runnerEnable(t, a, true)
	runnerState(t, runnerTick(a, "instance-a"))
	clearRunnerCooldowns(t, a)
	status := runnerState(t, runnerTick(a, "instance-a"))
	after := a.turnState.Status().Templates
	if planningCalls != 2 || len(status.Events) != 2 || status.Events[1].Result.Action != "fresh" || len(after) != 1 || !after[0].ExpiresAt.Equal(before[0].ExpiresAt) {
		t.Fatal("cooldown reset changed a fresh template or skipped the normal due check", status)
	}
}

func TestServerRunnerCooldownClearPreservesHourlyBudget(t *testing.T) {
	a, _ := runnerFixture(t)
	if err := a.turnState.Update([]byte(`{"enabled":true,"probe_hourly_limit":1}`)); err != nil {
		t.Fatal(err)
	}
	// Reserve one real manager attempt without contacting an upstream. Failed
	// credential access consumes budget and establishes a removable cooldown.
	if result, err := a.turnState.Probe("", "", func(string) (turnstate.Credential, error) {
		return turnstate.Credential{}, errors.New("dummy unavailable credential")
	}); err != nil || result.Action != "error" {
		t.Fatalf("initial failed probe=%+v err=%v", result, err)
	}
	planningCalls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		planningCalls++
		result, err := a.turnState.Probe("", "", func(string) (turnstate.Credential, error) {
			t.Fatal("cooldown clear bypassed the hourly collection budget")
			return turnstate.Credential{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return JSONResponse(http.StatusOK, result)
	}
	runnerEnable(t, a, true)
	runnerState(t, runnerTick(a, "instance-a"))
	response := a.clearTurnStateCooldowns(ManagementRequest{Body: []byte(`{"confirm":true}`)})
	var cleared struct {
		Cleared int `json:"cleared"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &cleared) != nil || cleared.Cleared == 0 {
		t.Fatal("test did not clear a real cooldown", string(response.Body))
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	budget := a.turnState.Status().ProbeBudget
	if planningCalls != 2 || len(status.Events) != 2 || status.Events[1].Result.Action != "budget_wait" || !budget.Exhausted || budget.Used != 1 {
		t.Fatal("cooldown clear changed the budget or skipped the normal budget check", status, budget)
	}
}
