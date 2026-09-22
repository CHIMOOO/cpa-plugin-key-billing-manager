package turnstate

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-key-billing/internal/messages"
)

const (
	firstExit  = "http://first.invalid:8080"
	secondExit = "http://second.invalid:8080"
	thirdExit  = "http://third.invalid:8080"
)

// refreshRoundFixture harvests account-a/model-a on the first of three static
// exits with the default 55-minute static cooldown. Each exit then answers
// what answers holds for it, a 292 by default.
func refreshRoundFixture(t *testing.T) (*Manager, *time.Time, map[string]ProbeResponse, *[]string) {
	t.Helper()
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30,"probe_proxies":["` + firstExit + `","` + secondExit + `","` + thirdExit + `"]}`)); err != nil {
		t.Fatal(err)
	}
	token := tokenAt(*now)
	answers := map[string]ProbeResponse{}
	used := &[]string{}
	m.runProbe = func(_ Credential, _ string, proxy string) (ProbeResponse, error) {
		*used = append(*used, proxy)
		response, ok := answers[proxy]
		if !ok {
			return ProbeResponse{Status: 200, Value: token}, nil
		}
		if response.Status == 0 {
			return ProbeResponse{}, errors.New("dummy connection failure")
		}
		return response, nil
	}
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "harvested" || result.Exit != firstExit {
		t.Fatalf("harvest = %+v %v", result, err)
	}
	return m, now, answers, used
}

func refreshAttempts(t *testing.T, m *Manager) int {
	t.Helper()
	views := m.Status().Templates
	if len(views) != 1 {
		t.Fatalf("templates = %+v", views)
	}
	return views[0].RefreshAttempts
}

func probeOnce(t *testing.T, m *Manager, want, exit string) ProbeResult {
	t.Helper()
	result, err := m.Probe("account-a", "model-a", dummyCredential)
	if err != nil || result.Action != want || exit != "" && result.Exit != exit {
		t.Fatalf("probe = %+v %v, want %s on %q", result, err, want, exit)
	}
	return result
}

// A 312 moves the round on promptly to the next exit without cooling the
// one that answered it; the first 292 ends the round, and the next round
// starts on the exit that served it.
func TestRefreshRoundMovesOnAfterA312AndStopsAtThe292(t *testing.T) {
	m, now, answers, used := refreshRoundFixture(t)
	degraded := ProbeResponse{Status: 200, Value: strings.Repeat("d", 312)}
	answers[firstExit] = degraded
	*now = now.Add(31 * time.Second)
	probeOnce(t, m, "degraded", firstExit)
	if rest := m.state.Cooldowns[proxyKey("account-a", "model-a", firstExit, false)]; rest.Until.After(*now) {
		t.Fatalf("a 312 during a refresh cooled its exit until %v", rest.Until)
	}
	if got := refreshAttempts(t, m); got != 1 {
		t.Fatalf("refresh attempts = %d", got)
	}
	*now = now.Add(2 * time.Second)
	probeOnce(t, m, "unchanged", secondExit)
	if got := refreshAttempts(t, m); got != 0 {
		t.Fatalf("the 292 did not end the round: %d attempts", got)
	}
	answers[secondExit] = degraded
	*now = now.Add(29 * time.Second)
	if result, _ := m.Probe("account-a", "model-a", dummyCredential); result.Action != "fresh" {
		t.Fatalf("the next round started before the refresh interval: %+v", result)
	}
	*now = now.Add(time.Second)
	probeOnce(t, m, "degraded", secondExit)
	if want := []string{firstExit, firstExit, secondExit, secondExit}; strings.Join(*used, " ") != strings.Join(want, " ") {
		t.Fatalf("exits = %v, want %v", *used, want)
	}
}

// A round without a 292 tries every exit once, then waits the retry interval.
func TestRefreshRoundWithoutA292TriesEachExitOnceThenWaits(t *testing.T) {
	m, now, answers, used := refreshRoundFixture(t)
	for _, exit := range []string{firstExit, secondExit, thirdExit} {
		answers[exit] = ProbeResponse{Status: 200, Value: strings.Repeat("d", 312)}
	}
	*now = now.Add(31 * time.Second)
	for attempt, exit := range []string{firstExit, secondExit, thirdExit} {
		probeOnce(t, m, "degraded", exit)
		if want := (attempt + 1) % 3; refreshAttempts(t, m) != want {
			t.Fatalf("attempt %d: refresh attempts = %d, want %d", attempt+1, refreshAttempts(t, m), want)
		}
		*now = now.Add(2 * time.Second)
	}
	ended := now.Add(-2 * time.Second)
	if result, _ := m.Probe("account-a", "model-a", dummyCredential); result.Action != "fresh" || !result.NextCheckAt.Equal(ended.Add(300*time.Second)) {
		t.Fatalf("an exhausted round did not wait the retry interval: %+v", result)
	}
	*now = ended.Add(300 * time.Second)
	probeOnce(t, m, "degraded", firstExit)
	if len(*used) != 5 {
		t.Fatalf("exits = %v", *used)
	}
	cfg := DefaultConfig()
	cfg.ProbeProxies = make([]string, 30)
	if refreshRoundLimit(cfg) != 25 || refreshRoundLimit(DefaultConfig()) != 1 {
		t.Fatal("a refresh round is not limited to 25 exits")
	}
}

// Only a failure of the exit itself cools a static exit during a refresh.
func TestRefreshExitFailuresCoolTheStaticExit(t *testing.T) {
	m, now, answers, _ := refreshRoundFixture(t)
	answers[firstExit] = ProbeResponse{}
	answers[secondExit] = ProbeResponse{Status: 403, ExitBlocked: true}
	*now = now.Add(31 * time.Second)
	probeOnce(t, m, "error", firstExit)
	if rest := m.state.Cooldowns[proxyKey("account-a", "model-a", firstExit, false)]; !rest.Until.Equal(now.Add(55 * time.Minute)) {
		t.Fatalf("a transport failure did not cool its exit: %+v", rest)
	}
	*now = now.Add(2 * time.Second)
	probeOnce(t, m, "error", secondExit)
	if rest := m.state.Cooldowns[proxyKey("account-a", "model-a", secondExit, false)]; !rest.Until.Equal(now.Add(55 * time.Minute)) {
		t.Fatalf("a blocked exit was not cooled: %+v", rest)
	}
	if got := refreshAttempts(t, m); got != 2 {
		t.Fatalf("refresh attempts = %d", got)
	}
	*now = now.Add(2 * time.Second)
	probeOnce(t, m, "unchanged", thirdExit)
}

// If the exits a round has not tried disappear, the round starts over on a
// tried exit instead of stalling until the template's renewal.
func TestRefreshRoundStartsOverWhenItsUntriedExitsDisappear(t *testing.T) {
	m, now, answers, _ := refreshRoundFixture(t)
	answers[firstExit] = ProbeResponse{Status: 200, Value: strings.Repeat("d", 312)}
	*now = now.Add(31 * time.Second)
	probeOnce(t, m, "degraded", firstExit)
	if err := m.Update([]byte(`{"probe_proxies":["` + firstExit + `"]}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	probeOnce(t, m, "degraded", firstExit)
	if got := refreshAttempts(t, m); got != 0 {
		t.Fatalf("a round over a single exit kept going: %d attempts", got)
	}
	if result, _ := m.Probe("account-a", "model-a", dummyCredential); result.Action != "fresh" || !result.NextCheckAt.Equal(now.Add(300*time.Second)) {
		t.Fatalf("the restarted round did not wait the retry interval: %+v", result)
	}
}

// Each account probes one bucket at a time: a second slot goes to another
// account rather than to the busy account's other model.
func TestParallelProbesRunOneLanePerAccount(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"],"models":["model-a","model-b"]}`)); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 4)
	release := make(chan struct{})
	m.runProbe = func(credential Credential, model, _ string) (ProbeResponse, error) {
		started <- credential.AccountID + "/" + model
		<-release
		return ProbeResponse{Status: 502}, nil
	}
	fetch := func(account string) (Credential, error) {
		return Credential{AccessToken: "dummy-oauth-token", AccountID: account}, nil
	}
	var wg sync.WaitGroup
	for _, want := range []string{"account-a/model-a", "account-b/model-a"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.ProbeConcurrently(3, nil, fetch); err != nil {
				t.Error(err)
			}
		}()
		if got := <-started; got != want {
			t.Fatalf("lane = %s, want %s", got, want)
		}
	}
	if result, err := m.ProbeConcurrently(3, nil, fetch); err != nil || result.Account != "" || result.Action != "cooling" {
		t.Fatalf("a busy account started a second probe: %+v %v", result, err)
	}
	progress := m.ProbeProgress()
	if !progress.Active || len(progress.Results) != 2 || progress.Results[0].Account != "account-a" || progress.Results[1].Account != "account-b" ||
		progress.Result.Account != "account-b" || m.Status().ProbeProgress.Results[1].Account != "account-b" {
		t.Fatalf("progress = %+v", progress)
	}
	close(release)
	wg.Wait()
	idle, _ := json.Marshal(m.ProbeProgress())
	if !strings.Contains(string(idle), `"active":false`) || !strings.Contains(string(idle), `"results":[]`) {
		t.Fatalf("idle progress = %s", idle)
	}
}

func TestCookieRetryDefaultsValidationAndLegacyFiles(t *testing.T) {
	if cfg := DefaultConfig(); cfg.CookieRetrySeconds != 300 || cfg.CookieRefreshAll {
		t.Fatalf("defaults = %+v", cfg)
	}
	m, _ := newTestManager(t)
	// Files written before v0.1.13 omit both fields.
	raw, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &file) != nil || json.Unmarshal(file["config"], &cfg) != nil {
		t.Fatal("cannot decode the state file")
	}
	if _, ok := cfg["cookie_retry_seconds"]; !ok {
		t.Fatal("the state file does not save the retry interval")
	}
	delete(cfg, "cookie_retry_seconds")
	delete(cfg, "cookie_refresh_all")
	file["config"], _ = json.Marshal(cfg)
	raw, _ = json.Marshal(file)
	if err := os.WriteFile(m.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	if got := m.Status().Config; got.CookieRetrySeconds != 300 || got.CookieRefreshAll {
		t.Fatalf("legacy file = %+v", got)
	}
	for _, value := range []string{"0", "29", "3601"} {
		err := m.Update([]byte(`{"cookie_retry_seconds":` + value + `}`))
		if detail := messages.FromError(err); detail.Key != "backend.turn_state_cookie_retry_invalid" || detail.Text != "Cookie retry interval must be between 30 and 3600 seconds" {
			t.Fatalf("retry %s = %v", value, err)
		}
	}
	if err := m.Update([]byte(`{"cookie_retry_seconds":600,"cookie_refresh_all":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	if got := m.Status().Config; got.CookieRetrySeconds != 600 || !got.CookieRefreshAll {
		t.Fatalf("a partial update or restart lost the cookie settings: %+v", got)
	}
}
