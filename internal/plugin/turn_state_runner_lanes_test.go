package plugin

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/turnstate"
)

// laneClock is the runner clock shared by the test and its lanes.
type laneClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *laneClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *laneClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func (c *laneClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func laneFixture(t *testing.T, parallel int) (*App, *laneClock) {
	t.Helper()
	a, now := runnerFixture(t)
	if err := a.turnState.Update(mustMarshal(t, map[string]int{"probe_parallel": parallel})); err != nil {
		t.Fatal(err)
	}
	clock := &laneClock{at: *now}
	a.turnStateRunner.now = clock.now
	a.turnStateRunner.sleep = func(time.Duration) { t.Error("an idle lane waited unexpectedly") }
	runnerEnable(t, a, true)
	return a, clock
}

func laneResult(action, account string, next time.Time) ManagementResponse {
	result := turnstate.ProbeResult{Action: action, NextCheckAt: next}
	if account != "" {
		result.Account, result.Model = account, "dummy-model"
	}
	return JSONResponse(http.StatusOK, result)
}

func TestServerRunnerLaneSlowProbeDoesNotBlockOtherLanes(t *testing.T) {
	a, clock := laneFixture(t, 2)
	start := clock.now()
	release, idle := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var slept atomic.Int64
	a.turnStateRunner.sleep = func(d time.Duration) {
		slept.Add(int64(d))
		clock.add(d)
	}
	a.turnStateRunner.probeLimit = func(limit int) ManagementResponse {
		if limit != 2 {
			t.Errorf("lane probe limit=%d, want the configured parallel slots", limit)
		}
		switch calls.Add(1) {
		case 1:
			<-release
			return laneResult("harvested", "slow-account", clock.now().Add(2*time.Second))
		case 2:
			clock.set(start.Add(time.Second))
			return laneResult("harvested", "fast-account", clock.now().Add(2*time.Second))
		case 3:
			// The fast account is paced while the slow lane still runs.
			return laneResult("account_wait", "", clock.now().Add(time.Second))
		case 4:
			return laneResult("harvested", "fast-account", clock.now().Add(2*time.Second))
		case 5:
			close(idle)
			fallthrough
		default:
			return laneResult("fresh", "", clock.now().Add(time.Hour))
		}
	}
	done := make(chan ManagementResponse, 1)
	go func() { done <- runnerTick(a, "instance-a") }()
	<-idle
	running := a.turnStateRunner.status()
	if !running.InFlight || running.Phase != "running" || len(running.Events) != 2 {
		t.Fatal("a slow lane held back the other lane's results", running)
	}
	for i, event := range running.Events {
		at := start.Add(time.Duration(i+1) * time.Second)
		if event.Result.Account != "fast-account" || !event.At.Equal(at) || !event.Result.NextCheckAt.Equal(at.Add(2*time.Second)) {
			t.Fatal("a lane result was not logged with its own completion time", event)
		}
	}
	if slept.Load() != int64(time.Second) {
		t.Fatal("an idle lane did not wait for its paced account while another lane ran", time.Duration(slept.Load()))
	}
	clock.set(start.Add(10 * time.Second))
	close(release)
	status := runnerState(t, <-done)
	if status.InFlight || len(status.Events) != 3 || status.LastError != "" {
		t.Fatal("finalizer repeated logged results or added an idle entry", status)
	}
	slow := status.Events[2]
	if slow.Result.Account != "slow-account" || !slow.At.Equal(start.Add(10*time.Second)) {
		t.Fatal("the slow probe lost its own completion time", slow)
	}
	for i, event := range status.Events {
		if event.Sequence != uint64(i+1) {
			t.Fatal("lane events are out of order", status.Events)
		}
	}
	if !status.NextCheckAt.Equal(start.Add(12 * time.Second)) {
		t.Fatal("next check did not follow the earliest hint within the heartbeat bounds", status.NextCheckAt)
	}
}

func TestServerRunnerLanesDriveManagerUntilNothingIsDue(t *testing.T) {
	a, _ := laneFixture(t, 2)
	if err := a.turnState.Update([]byte(`{"probe_accounts":["dummy-account","dummy-other"]}`)); err != nil {
		t.Fatal(err)
	}
	a.turnStateRunner.now = time.Now
	// A lane may briefly wait while the other account's probe is in flight.
	a.turnStateRunner.sleep = func(time.Duration) { time.Sleep(time.Millisecond) }
	var fetches atomic.Int32
	a.turnStateRunner.probeLimit = func(limit int) ManagementResponse {
		result, err := a.turnState.ProbeConcurrently(limit, nil, func(string) (turnstate.Credential, error) {
			fetches.Add(1)
			return turnstate.Credential{}, errors.New("dummy unavailable credential")
		})
		if err != nil {
			t.Error(err)
			return JSONError(http.StatusInternalServerError, "probe_failed", "dummy probe failure")
		}
		return JSONResponse(http.StatusOK, result)
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	// Each failed credential pauses its account, after which nothing is due.
	accounts := map[string]bool{}
	for _, event := range status.Events {
		accounts[event.Result.Account] = event.Result.Action == "error"
	}
	if fetches.Load() != 2 || len(status.Events) != 2 || !accounts["dummy-account"] || !accounts["dummy-other"] || status.InFlight {
		t.Fatal("lanes did not give each due account one probe and then return", fetches.Load(), status)
	}
}

func TestServerRunnerLaneStopPreventsNewProbes(t *testing.T) {
	a, clock := laneFixture(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	sleeping, wake := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	a.turnStateRunner.sleep = func(time.Duration) {
		close(sleeping)
		<-wake
	}
	a.turnStateRunner.probeLimit = func(int) ManagementResponse {
		switch calls.Add(1) {
		case 1:
			close(entered)
			<-release
			return laneResult("harvested", "dummy-account", clock.now().Add(2*time.Second))
		case 2:
			return laneResult("cooling", "", clock.now().Add(time.Second))
		default:
			t.Error("a lane started a probe after Stop")
			return laneResult("fresh", "", clock.now().Add(time.Hour))
		}
	}
	done := make(chan ManagementResponse, 1)
	go func() { done <- runnerTick(a, "instance-a") }()
	<-entered
	<-sleeping
	if status := runnerEnable(t, a, false); status.Enabled || status.Phase != "draining" {
		t.Fatal("Stop waited for the lanes", status)
	}
	close(wake)
	close(release)
	status := runnerState(t, <-done)
	if calls.Load() != 2 || status.InFlight || status.Phase != "stopped" || len(status.Events) != 1 ||
		status.Events[0].Result.Account != "dummy-account" || !status.NextCheckAt.IsZero() {
		t.Fatal("Stop did not end the lanes after their running probe", calls.Load(), status)
	}
}

func TestServerRunnerLaneWindowBoundsNewStarts(t *testing.T) {
	a, clock := laneFixture(t, 2)
	a.turnStateRunner.laneWindow = 500 * time.Millisecond
	var calls atomic.Int32
	a.turnStateRunner.probeLimit = func(int) ManagementResponse {
		switch calls.Add(1) {
		case 1:
			// This probe outlasts the start window.
			time.Sleep(700 * time.Millisecond)
			return laneResult("harvested", "dummy-account", clock.now().Add(2*time.Second))
		case 2:
			// Another lane runs, but this wait would end after the window.
			return laneResult("cooling", "", clock.now().Add(time.Second))
		default:
			t.Error("a lane started a probe after the start window")
			return laneResult("fresh", "", clock.now().Add(time.Hour))
		}
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	if calls.Load() != 2 || status.InFlight || len(status.Events) != 1 || status.Events[0].Result.Account != "dummy-account" {
		t.Fatal("the start window did not bound the lanes", calls.Load(), status)
	}
}

func TestServerRunnerIdleLaneDoesNotWaitWithoutRunningProbe(t *testing.T) {
	a, clock := laneFixture(t, 1)
	start := clock.now()
	var calls atomic.Int32
	a.turnStateRunner.probeLimit = func(limit int) ManagementResponse {
		calls.Add(1)
		if limit != 1 {
			t.Errorf("lane probe limit=%d, want one slot", limit)
		}
		return laneResult("account_wait", "", clock.now().Add(time.Second))
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	if calls.Load() != 1 || status.InFlight || len(status.Events) != 1 || status.Events[0].Result.Action != "account_wait" {
		t.Fatal("an idle lane kept the tick open without a running probe", calls.Load(), status)
	}
	if !status.NextCheckAt.Equal(start.Add(2*time.Second)) || !status.Events[0].Result.NextCheckAt.Equal(status.NextCheckAt) {
		t.Fatal("the idle result did not schedule the next heartbeat", status)
	}
}

func TestServerRunnerLanePanicFinalizes(t *testing.T) {
	for _, logged := range []bool{false, true} {
		name := "before-any-probe"
		if logged {
			name = "after-a-logged-probe"
		}
		t.Run(name, func(t *testing.T) {
			a, clock := laneFixture(t, 2)
			var calls atomic.Int32
			a.turnStateRunner.probeLimit = func(int) ManagementResponse {
				if calls.Add(1) == 1 && logged {
					return laneResult("harvested", "dummy-account", clock.now().Add(2*time.Second))
				}
				panic("simulated lane panic")
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected the lane panic to reach the App recovery boundary")
					}
				}()
				_ = runnerTick(a, "instance-a")
			}()
			status := a.turnStateRunner.status()
			if status.InFlight || !status.Enabled || len(status.Events) != 1 || status.NextCheckAt.IsZero() {
				t.Fatal("a lane panic left an active flag or lost the schedule", status)
			}
			if logged && (status.Events[0].Result.Account != "dummy-account" || status.LastError != "") {
				t.Fatal("finalizer replaced the logged probe after a panic", status)
			}
			if !logged && (status.Events[0].Result.Action != "error" || status.LastError == "") {
				t.Fatal("a panic before any probe was not reported", status)
			}
			before := calls.Load()
			runnerState(t, runnerTick(a, "instance-a"))
			if calls.Load() != before {
				t.Fatal("the next heartbeat repeated a probe before next_check_at")
			}
		})
	}
}
