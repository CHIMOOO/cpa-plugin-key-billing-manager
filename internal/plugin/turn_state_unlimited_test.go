package plugin

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"cpa-key-billing/internal/turnstate"
)

func TestServerRunnerUnlimitedPolicyRechecksAndKeepsPacing(t *testing.T) {
	a, now := runnerFixture(t)
	calls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		calls++
		if calls == 1 {
			return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: "cooling", NextCheckAt: now.Add(time.Minute)})
		}
		return JSONResponse(http.StatusOK, turnstate.ProbeResult{
			Action: "degraded", ProxyPool: "rotating", ProxyAttempt: calls - 1,
			ProxyAttemptLimit: 0, NextCheckAt: *now,
		})
	}
	runnerEnable(t, a, true)
	runnerState(t, runnerTick(a, "instance-a"))
	*now = now.Add(5 * time.Second)
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 1 {
		t.Fatal("fixture did not retain the previous cooldown hint")
	}
	settings := []byte(`{"probe_static_cooldown_minutes":0,"probe_rotating_cooldown_minutes":0,"probe_account_cooldown_minutes":0,"probe_rotating_max_attempts":0,"probe_hourly_limit":0}`)
	if response := a.setTurnState(ManagementRequest{Body: settings}); response.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("saving unlimited policy failed or started a probe: %s", response.Body)
	}
	response := runnerTick(a, "instance-a")
	status := runnerState(t, response)
	if calls != 2 || !status.Enabled || !status.NextCheckAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("next heartbeat did not recheck with normal request spacing", calls, status)
	}
	// Zero and absent are different on the wire: old events without a limit
	// display the legacy default, while new unlimited events must show no cap.
	if !bytes.Contains(response.Body, []byte(`"proxy_attempt_limit":0`)) {
		t.Fatal("unlimited event lost its explicit zero limit", string(response.Body))
	}
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 2 {
		t.Fatal("unlimited mode bypassed the runner's ordinary request spacing")
	}
	*now = now.Add(2 * time.Second)
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 3 {
		t.Fatal("unlimited retry did not resume after ordinary spacing")
	}
	runnerEnable(t, a, false)
	if response := a.setTurnState(ManagementRequest{Body: settings}); response.StatusCode != http.StatusOK {
		t.Fatal("cannot save policy while stopped", string(response.Body))
	}
	*now = now.Add(time.Minute)
	status = runnerState(t, runnerTick(a, "instance-a"))
	if calls != 3 || status.Enabled {
		t.Fatal("saving unlimited policy restarted stopped collection", calls, status)
	}
}
