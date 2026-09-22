package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

func runnerFixture(t *testing.T) (*App, *time.Time) {
	t.Helper()
	a := newConfiguredApp(t)
	if err := a.turnState.Update([]byte(`{"probe_accounts":["dummy-account"],"models":["dummy-model"]}`)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	a.turnStateRunner.now = func() time.Time { return now }
	return a, &now
}

func runnerState(t *testing.T, response ManagementResponse) turnStateRunnerStatus {
	t.Helper()
	var status turnStateRunnerStatus
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &status) != nil {
		t.Fatalf("runner response=%d %s", response.StatusCode, response.Body)
	}
	return status
}

func runnerEnable(t *testing.T, a *App, enabled bool) turnStateRunnerStatus {
	t.Helper()
	return runnerState(t, a.setTurnStateRunner(ManagementRequest{Body: mustMarshal(t, map[string]bool{"enabled": enabled})}))
}

func runnerTick(a *App, instance string) ManagementResponse {
	raw, _ := json.Marshal(map[string]string{"instance_id": instance, "version": "audit-0.0.10"})
	return a.tickTurnStateRunner(ManagementRequest{Body: raw})
}

func TestServerRunnerUsesDueHintWithoutAddingIdleTimeBetweenBuckets(t *testing.T) {
	a, now := runnerFixture(t)
	runnerEnable(t, a, true)
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONResponse(200, turnstate.ProbeResult{Action: "harvested", NextCheckAt: now.Add(2 * time.Second)})
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	if status.NextTickSeconds != 2 {
		t.Fatalf("ready buckets unnecessarily wait %d seconds", status.NextTickSeconds)
	}
	*now = now.Add(1500 * time.Millisecond)
	if status := a.turnStateRunner.status(); status.NextTickSeconds != 1 {
		t.Fatalf("fractional due delay must round up: %+v", status)
	}
	runnerEnable(t, a, false)
	if status := a.turnStateRunner.status(); status.NextTickSeconds != 5 {
		t.Fatalf("stopped collector should keep an idle heartbeat: %+v", status)
	}
}

func TestServerRunnerControlPersistsButLeaseAndLogsResetOnRestart(t *testing.T) {
	a, now := runnerFixture(t)
	if initial := runnerState(t, a.getTurnStateRunner(ManagementRequest{})); initial.Enabled || initial.Online || initial.Phase != "stopped" {
		t.Fatal("new collector enabled automatically", initial)
	}
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONResponse(200, turnstate.ProbeResult{Action: "fresh", NextCheckAt: now.Add(time.Minute)})
	}
	if status := runnerEnable(t, a, true); !status.Enabled || status.Phase != "offline" || status.Revision != 1 {
		t.Fatal("offline collector lost durable intent", status)
	}
	first := runnerState(t, runnerTick(a, "instance-a"))
	if !first.Online || first.InFlight || first.Phase != "waiting" || len(first.Events) != 1 || first.Events[0].Result.Action != "fresh" {
		t.Fatal("completed tick did not report its final event", first)
	}
	statePath := strings.TrimSuffix(a.turnStateRunner.path, ".turn-state-runner.json")
	restarted := newTestApp(t)
	t.Cleanup(restarted.Shutdown)
	config := []byte("enabled: true\nstate_file: " + strconv.Quote(statePath) + "\n")
	if err := restarted.configure(mustMarshal(t, LifecycleRequest{ConfigYAML: config})); err != nil {
		t.Fatal(err)
	}
	status := restarted.turnStateRunner.status()
	if !status.Enabled || status.Online || status.InstanceID != "" || len(status.Events) != 0 || status.Epoch == first.Epoch || status.Revision != 1 {
		t.Fatal("restart did not preserve intent/reset process lease", status)
	}
	if status := runnerEnable(t, restarted, false); status.Enabled || status.Revision != 2 {
		t.Fatal("durable Stop failed", status)
	}
	var stored turnStateRunnerControl
	raw, _ := os.ReadFile(restarted.turnStateRunner.path)
	if json.Unmarshal(raw, &stored) != nil || stored.Enabled || stored.Revision != 2 {
		t.Fatal("Stop did not reach disk")
	}
}

func TestServerRunnerLeaseDueAndConfigurationChange(t *testing.T) {
	a, now := runnerFixture(t)
	calls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		calls++
		return JSONResponse(200, turnstate.ProbeResult{Action: "fresh", NextCheckAt: now.Add(time.Minute)})
	}
	runnerEnable(t, a, true)
	runnerState(t, runnerTick(a, "instance-a"))
	*now = now.Add(5 * time.Second)
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 1 {
		t.Fatal("heartbeat ran a fresh bucket")
	}
	if response := runnerTick(a, "instance-b"); response.StatusCode != http.StatusConflict || calls != 1 {
		t.Fatal("second collector stole a live lease", string(response.Body))
	}
	if err := a.turnState.Update([]byte(`{"models":["another-model"]}`)); err != nil {
		t.Fatal(err)
	}
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 2 {
		t.Fatal("configuration change did not make the next tick due")
	}
	*now = now.Add(91 * time.Second)
	status := runnerState(t, runnerTick(a, "instance-b"))
	if status.InstanceID != "instance-b" || calls != 3 {
		t.Fatal("expired collector lease was not recoverable", status)
	}
}

func TestServerRunnerStopDrainsWithoutBlockingOrReentering(t *testing.T) {
	a, now := runnerFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	a.turnStateRunner.probe = func() ManagementResponse {
		calls.Add(1)
		close(entered)
		<-release
		return JSONResponse(200, turnstate.ProbeResult{Action: "harvested", Account: "dummy-account", Model: "dummy-model"})
	}
	runnerEnable(t, a, true)
	done := make(chan ManagementResponse, 1)
	go func() { done <- runnerTick(a, "instance-a") }()
	<-entered
	if response := runnerTick(a, "instance-a"); response.StatusCode != http.StatusConflict {
		t.Fatal("same collector entered twice")
	}
	// Advancing the test clock simulates a stuck transport outliving its lease;
	// an active synchronous call still cannot be stolen by another instance.
	*now = now.Add(91 * time.Second)
	if response := runnerTick(a, "instance-b"); response.StatusCode != http.StatusConflict {
		t.Fatal("expired lease allowed concurrent probe")
	}
	status := runnerEnable(t, a, false)
	if status.Enabled || !status.InFlight || status.Phase != "draining" {
		t.Fatal("Stop waited for or canceled the active probe", status)
	}
	if response := a.probeTurnState(ManagementRequest{Body: []byte(`{}`)}); response.StatusCode != 409 {
		t.Fatal("manual probe overlapped a draining server probe")
	}
	close(release)
	status = runnerState(t, <-done)
	if status.Enabled || status.InFlight || status.Phase != "stopped" || len(status.Events) != 1 {
		t.Fatal("finished old probe re-enabled stopped collection", status)
	}
	runnerState(t, runnerTick(a, "instance-b"))
	if calls.Load() != 1 {
		t.Fatal("disabled collector launched another probe")
	}
}

func TestServerRunnerManualProbeAndConfigureCannotOverlap(t *testing.T) {
	a, _ := runnerFixture(t)
	finish, allowed := a.beginManualTurnStateProbe()
	if !allowed {
		t.Fatal("stopped runner rejected manual probe")
	}
	runnerEnable(t, a, true)
	if response := runnerTick(a, "instance-a"); response.StatusCode != 409 {
		t.Fatal("starting server mode overlapped manual probe")
	}
	finish()
	entered, release := make(chan struct{}), make(chan struct{})
	a.turnStateRunner.probe = func() ManagementResponse {
		close(entered)
		<-release
		return JSONResponse(200, turnstate.ProbeResult{Action: "fresh"})
	}
	done := make(chan ManagementResponse, 1)
	go func() { done <- runnerTick(a, "instance-a") }()
	<-entered
	oldEpoch := a.turnStateRunner.status().Epoch
	newPath := filepath.Join(t.TempDir(), "other.db")
	configured := make(chan error, 1)
	go func() {
		configured <- a.configure(mustMarshal(t, LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: " + strconv.Quote(newPath) + "\n")}))
	}()
	select {
	case err := <-configured:
		t.Fatal("configure overtook active tick", err)
	case <-time.After(20 * time.Millisecond):
	}
	// This also verifies a queued exclusive Configure gate cannot prevent Stop.
	if status := runnerEnable(t, a, false); status.Phase != "draining" {
		t.Fatal("Stop could not pass queued reconfiguration", status)
	}
	close(release)
	runnerState(t, <-done)
	if err := <-configured; err != nil {
		t.Fatal(err)
	}
	status := a.turnStateRunner.status()
	if status.Enabled || status.InFlight || status.Epoch == oldEpoch || len(status.Events) != 0 {
		t.Fatal("old probe contaminated new installation state", status)
	}
}

func TestServerRunnerFailuresRetryWithoutClearingIntentAndBoundLogs(t *testing.T) {
	a, now := runnerFixture(t)
	runnerEnable(t, a, true)
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONError(http.StatusInternalServerError, "unsafe-upstream", "https://dummy-secret:dummy-token@upstream.invalid")
	}
	status := runnerState(t, runnerTick(a, "instance-a"))
	if !status.Enabled || status.LastError == "" || strings.Contains(string(mustMarshal(t, status)), "dummy-secret") {
		t.Fatal("failed tick lost intent or leaked an opaque failure", status)
	}
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONResponse(200, turnstate.ProbeResult{Action: "degraded", Exit: "socks5://dummy-user:dummy-secret@proxy.invalid:3000", Reason: "A degraded-length state was received and not saved; the next probe follows the exit pool retry rules"})
	}
	for i := 0; i < 110; i++ {
		*now = now.Add(31 * time.Second)
		status = runnerState(t, runnerTick(a, "instance-a"))
	}
	raw := mustMarshal(t, status)
	if len(status.Events) != 100 || status.Events[0].Sequence != 12 || status.Events[99].Sequence != 111 || status.LastError != "" || !bytes.Contains(raw, []byte("socks5://dummy-user:dummy-secret@proxy.invalid:3000")) {
		t.Fatal("event ring was not bounded, ordered, or retaining complete management proxy addresses", string(raw))
	}
}

func TestServerRunnerControlValidationAndFailedSaveRollback(t *testing.T) {
	a, _ := runnerFixture(t)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":true,"unknown":1}`} {
		if response := a.setTurnStateRunner(ManagementRequest{Body: []byte(body)}); response.StatusCode != 400 {
			t.Fatal("accepted invalid runner control", body)
		}
	}
	a.turnStateRunner.writeControl = func(string, any) error { return errors.New("dummy-secret-network-error") }
	response := a.setTurnStateRunner(ManagementRequest{Body: []byte(`{"enabled":true}`)})
	if response.StatusCode != 500 || a.turnStateRunner.status().Enabled || bytes.Contains(response.Body, []byte("dummy-secret")) {
		t.Fatal("failed save changed desired state or exposed filesystem details")
	}
	a.turnStateRunner.writeControl = nil
	if err := a.turnState.Update([]byte(`{"probe_accounts":[]}`)); err != nil {
		t.Fatal(err)
	}
	if response := a.setTurnStateRunner(ManagementRequest{Body: []byte(`{"enabled":true}`)}); response.StatusCode != 400 {
		t.Fatal("started server collection without a configured account scope")
	}
}

func TestServerRunnerProxyLogsRemainCompleteAndPrivate(t *testing.T) {
	a, _ := runnerFixture(t)
	runnerEnable(t, a, true)
	const proxy = "socks5://dummy-session-b:dummy%40password@[2001:db8::1]:3000"
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: "harvested", Exit: proxy})
	}
	runnerState(t, runnerTick(a, "instance-a"))
	for _, path := range []string{routeTurnStateRunner, routeTurnState, routeTurnStateProbeProgress} {
		response := a.routeManagement(ManagementRequest{Method: http.MethodGet}, path)
		if response.StatusCode != http.StatusOK || response.Headers.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("management probe logs may be cached: %s %d %v", path, response.StatusCode, response.Headers)
		}
		if path != routeTurnStateProbeProgress && !bytes.Contains(response.Body, []byte(proxy)) {
			t.Fatalf("management log lost the exact session-specific proxy: %s", path)
		}
	}
	for _, value := range []string{"javascript:dummy-secret", "http://", "http://dummy:dummy-secret@proxy.invalid\nforged-line"} {
		if got := sanitizeRunnerResult(turnstate.ProbeResult{Exit: value}); got.Exit != "invalid proxy" {
			t.Fatalf("accepted an invalid proxy log address: %+v", got)
		}
	}
}

func TestServerRunnerCompleteProxyLogsFitCollectorResponseLimit(t *testing.T) {
	a, now := runnerFixture(t)
	runnerEnable(t, a, true)
	// Valid proxy userinfo can grow sixfold in JSON. The collector must still
	// receive complete, readable status instead of entering error backoff.
	proxy := "socks5://dummy-session:" + strings.Repeat("&", 4000) + "@proxy.invalid:3000"
	if err := a.turnState.Update(mustMarshal(t, map[string]any{"probe_proxies": []string{proxy}})); err != nil {
		t.Fatal(err)
	}
	a.turnStateRunner.probe = func() ManagementResponse {
		return JSONResponse(http.StatusOK, turnstate.ProbeResult{Action: "harvested", Exit: proxy})
	}
	var status turnStateRunnerStatus
	for i := 0; i < 110; i++ {
		*now = now.Add(31 * time.Second)
		response := runnerTick(a, "instance-a")
		if len(response.Body) >= 2<<20 {
			t.Fatal("complete proxy logs exceeded the installed collector response limit")
		}
		status = runnerState(t, response)
	}
	if len(status.Events) == 0 || len(status.Events) >= 100 || status.Events[len(status.Events)-1].Sequence != 110 {
		t.Fatal("byte-limited log did not retain the most recent events in order")
	}
	for i, event := range status.Events {
		if event.Result.Exit != proxy || (i > 0 && event.Sequence != status.Events[i-1].Sequence+1) {
			t.Fatal("byte pruning truncated an address or broke event ordering")
		}
	}
}

func TestServerRunnerPanicFinalizesAndLostResponseDoesNotRepeatProbe(t *testing.T) {
	a, now := runnerFixture(t)
	runnerEnable(t, a, true)
	calls := 0
	a.turnStateRunner.probe = func() ManagementResponse {
		calls++
		panic("simulated callback panic")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected callback panic to reach the App recovery boundary")
			}
		}()
		_ = runnerTick(a, "instance-a")
	}()
	status := a.turnStateRunner.status()
	if status.InFlight || !status.Enabled || len(status.Events) != 1 || status.LastError == "" {
		t.Fatal("panic left an active flag or lost persistent intent", status)
	}
	// Pretend the client never received the completed failure response. The
	// backend owns due state, so its next heartbeat must not repeat that call.
	*now = now.Add(5 * time.Second)
	runnerState(t, runnerTick(a, "instance-a"))
	if calls != 1 {
		t.Fatal("lost tick response repeated an attempt before next_check_at")
	}
	a.turnStateRunner.probe = func() ManagementResponse {
		calls++
		return JSONResponse(200, turnstate.ProbeResult{Action: "fresh"})
	}
	*now = now.Add(30 * time.Second)
	if status := runnerState(t, runnerTick(a, "instance-a")); status.InFlight || calls != 2 || status.LastError != "" {
		t.Fatal("collector failed to recover after callback panic", status)
	}
}

func TestServerRunnerPendingStopDoesNotAdmitAnotherProbe(t *testing.T) {
	a, _ := runnerFixture(t)
	runnerEnable(t, a, true)
	entered, release := make(chan struct{}), make(chan struct{})
	a.turnStateRunner.writeControl = func(path string, value any) error {
		close(entered)
		<-release
		return writePrivateJSON(path, value)
	}
	a.turnStateRunner.probe = func() ManagementResponse {
		t.Fatal("tick sent a new request while durable Stop was being saved")
		return ManagementResponse{}
	}
	done := make(chan ManagementResponse, 1)
	go func() { done <- a.setTurnStateRunner(ManagementRequest{Body: []byte(`{"enabled":false}`)}) }()
	<-entered
	runnerState(t, runnerTick(a, "instance-a"))
	close(release)
	if status := runnerState(t, <-done); status.Enabled {
		t.Fatal("Stop did not become durable")
	}
}

func TestServerRunnerInvalidPersistentControlDoesNotSwitchStores(t *testing.T) {
	a, _ := runnerFixture(t)
	oldPath := a.turnState.StoragePath()
	for _, value := range []string{`{}`, `{"version":1,"enabled":null,"revision":1}`, `{"version":2,"enabled":true,"revision":1}`, `{"version":1,"enabled":true,"revision":1,"extra":true}`} {
		newPath := filepath.Join(t.TempDir(), "new.db")
		if err := os.WriteFile(newPath+".turn-state-runner.json", []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.configure(mustMarshal(t, LifecycleRequest{ConfigYAML: []byte("enabled: true\nstate_file: " + strconv.Quote(newPath) + "\n")})); err == nil || a.turnState.StoragePath() != oldPath {
			t.Fatal("invalid runner state partially switched storage", value, err)
		}
	}
}

func TestServerRunnerStopDoesNotWaitForReferencePriceDownload(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	a := newApp(billing.NewStore(openRepository, func(context.Context) ([]byte, error) {
		close(entered)
		<-release
		return nil, errors.New("simulated reference-price outage")
	}))
	t.Cleanup(a.Shutdown)
	done := make(chan error, 1)
	config := mustMarshal(t, LifecycleRequest{ConfigYAML: testConfigYAML(t, true)})
	go func() { done <- a.configure(config) }()
	<-entered
	if err := a.turnState.Update([]byte(`{"probe_accounts":["dummy-account"],"models":["dummy-model"]}`)); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		runnerEnable(t, a, true)
		runnerEnable(t, a, false)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("reference-price download held the runner control gate")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
