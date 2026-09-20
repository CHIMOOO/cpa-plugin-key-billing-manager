package turnstate

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeHourlyLimitDefaultsAndValidation(t *testing.T) {
	if cfg := DefaultConfig(); cfg.ProbeHourlyLimit != 0 || cfg.ProbeVerifyCompletion {
		t.Fatalf("new controls unexpectedly enabled: %+v", cfg)
	}
	m, _ := newTestManager(t)
	for _, raw := range []string{`{"probe_hourly_limit":-1}`, `{"probe_hourly_limit":10001}`, `{"probe_hourly_limit":1.5}`} {
		if err := m.Update([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid limit: %s", raw)
		}
	}
	for _, raw := range []string{`{"probe_hourly_limit":1}`, `{"probe_hourly_limit":10000}`, `{"probe_hourly_limit":0}`} {
		if err := m.Update([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeBudgetRollingWindowAndConservativeSubsecondRelease(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 100_000_000, time.UTC)
	state := diskState{Config: Config{ProbeHourlyLimit: 2}}
	state.ProbeUsage = reserveProbeUsage(state.ProbeUsage, start)
	state.ProbeUsage = reserveProbeUsage(state.ProbeUsage, start.Add(800*time.Millisecond))
	if len(state.ProbeUsage) != 1 || state.ProbeUsage[0].Count != 2 {
		t.Fatalf("same-second requests were not coalesced: %+v", state.ProbeUsage)
	}
	wantResume := start.Add(time.Hour + 800*time.Millisecond)
	budget := probeBudget(state, start.Add(time.Hour))
	if !budget.Exhausted || budget.Used != 2 || !budget.ResumesAt.Equal(wantResume) {
		t.Fatalf("budget prematurely expired an entry: %+v", budget)
	}
	if got := probeBudget(state, wantResume); got.Exhausted || got.Used != 0 || got.Remaining != 2 {
		t.Fatalf("budget did not resume at the reported boundary: %+v", got)
	}
}

func TestProbeBudgetLimitChangesUseExistingHistory(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	state := diskState{Config: Config{ProbeHourlyLimit: 0}}
	for i := range 5 {
		state.ProbeUsage = reserveProbeUsage(state.ProbeUsage, start.Add(time.Duration(i)*time.Minute))
	}
	if got := probeBudget(state, start.Add(5*time.Minute)); got.Used != 5 || got.Exhausted || got.Remaining != 0 || !got.ResumesAt.IsZero() {
		t.Fatalf("unlimited mode lost usage or claimed a finite allowance: %+v", got)
	}
	state.Config.ProbeHourlyLimit = 3
	got := probeBudget(state, start.Add(5*time.Minute))
	if !got.Exhausted || !got.ResumesAt.Equal(start.Add(time.Hour+2*time.Minute)) {
		t.Fatalf("reduced limit must wait for three old reservations: %+v", got)
	}
	state.Config.ProbeHourlyLimit = 8
	if got := probeBudget(state, start.Add(5*time.Minute)); got.Exhausted || got.Remaining != 3 {
		t.Fatalf("increased limit did not admit remaining capacity: %+v", got)
	}
}

func TestProbeBudgetUnlimitedHistorySurvivesRestartAndProtectsRenewingTemplate(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"],"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	learn(t, m, tokenAt(now.Add(-56*time.Minute)))
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Action != "degraded" {
		t.Fatalf("unlimited probe failed: %+v, %v", result, err)
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if budget := loaded.Status().ProbeBudget; budget.Limit != 0 || budget.Used != 1 || budget.Exhausted {
		t.Fatalf("restart lost unlimited-mode history: %+v", budget)
	}
	if err := loaded.Update([]byte(`{"probe_hourly_limit":1}`)); err != nil {
		t.Fatal(err)
	}
	if result, err := loaded.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "budget_wait" {
		t.Fatalf("turning budget on ignored prior unlimited reservation: %+v, %v", result, err)
	}
	if headers, _ := loaded.Before("business-during-budget-wait", "account-a", "model-a", nil); headers.Get(Header) == "" {
		t.Fatal("exhausted probe budget blocked an unexpired business template")
	}
}

func TestProbeBudgetIsGlobalDurableAndNotResetByControls(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_hourly_limit":3,"probe_accounts":["account-a","account-b","account-c","account-d"],"models":["model-a","model-b"],"probe_proxies":["http://static.invalid:8000"],"probe_proxies_rotating":["socks5://rotating.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	calls, fetches := 0, 0
	fetch := func(account string) (Credential, error) { fetches++; return dummyCredential(account) }
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		calls++
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	for _, account := range []string{"account-a", "account-b"} {
		result, err := m.Probe(account, "model-a", fetch)
		if err != nil || result.Action != "degraded" {
			t.Fatalf("first pool probe: %+v, %v", result, err)
		}
		*now = now.Add(3 * time.Second)
	}
	if err := m.Update([]byte(`{"probe_proxies":[],"probe_proxies_rotating":[]}`)); err != nil {
		t.Fatal(err)
	}
	if result, err := m.Probe("account-c", "model-b", fetch); err != nil || result.ProxyPool != "direct" {
		t.Fatalf("direct probe: %+v, %v", result, err)
	}
	if result, err := m.Probe("account-d", "model-a", fetch); err != nil || result.Action != "budget_wait" || calls != 3 || fetches != 3 {
		t.Fatalf("manual probe bypassed the shared budget: %+v, %v calls=%d fetches=%d", result, err, calls, fetches)
	}
	if err := m.Clear("", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClearCooldowns("", ""); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"probe_hourly_limit":0,"enabled":false}`, `{"probe_hourly_limit":3,"enabled":true}`} {
		if err := m.Update([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	loaded.runProbe = m.runProbe
	if budget := loaded.Status().ProbeBudget; budget.Used != 3 || !budget.Exhausted {
		t.Fatalf("clear/reconfigure/restart lost global reservations: %+v", budget)
	}
	if result, err := loaded.Probe("account-d", "model-a", fetch); err != nil || result.Action != "budget_wait" || calls != 3 {
		t.Fatalf("restarted collector bypassed budget: %+v, %v", result, err)
	}
	*now = loaded.Status().ProbeBudget.ResumesAt
	if result, err := loaded.Probe("account-d", "model-a", fetch); err != nil || result.Action != "degraded" || calls != 4 {
		t.Fatalf("collector did not resume at budget boundary: %+v, %v calls=%d", result, err, calls)
	}
}

func TestProbeBudgetChargesFailuresAndReservesBeforeTransport(t *testing.T) {
	for _, kind := range []string{"credential", "transport", "finish-write"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := newTestManager(t)
			if err := m.Update([]byte(`{"probe_hourly_limit":1,"probe_accounts":["account-a","account-b"]}`)); err != nil {
				t.Fatal(err)
			}
			fetch := func(account string) (Credential, error) {
				if kind == "credential" {
					return Credential{}, errors.New("dummy credential failure")
				}
				return dummyCredential(account)
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				loaded := New()
				loaded.now = m.now
				if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil || loaded.Status().ProbeBudget.Used != 1 {
					t.Fatalf("transport started before durable reservation: %v", err)
				}
				if kind == "finish-write" {
					m.writeState = func(string, []byte) error { return errors.New("dummy write failure") }
				}
				return ProbeResponse{}, errors.New("dummy transport failure")
			}
			_, _ = m.Probe("account-a", "model-a", fetch)
			if budget := m.Status().ProbeBudget; budget.Used != 1 || !budget.Exhausted {
				t.Fatalf("failed attempt lost its reservation: %+v", budget)
			}
			if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Action != "budget_wait" {
				t.Fatalf("failure was incorrectly refunded: %+v, %v", result, err)
			}
		})
	}
}

func TestProbeBudgetFailedReservationSendsNothingAndKeepsBusinessAvailable(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_hourly_limit":1,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	learn(t, m, tokenAt(now.Add(-56*time.Minute)))
	entered, release := make(chan struct{}), make(chan struct{})
	m.writeState = func(string, []byte) error {
		close(entered)
		<-release
		return errors.New("dummy reservation write failure")
	}
	var fetches atomic.Int32
	done := make(chan error, 1)
	go func() {
		_, err := m.Probe("", "", func(account string) (Credential, error) { fetches.Add(1); return dummyCredential(account) })
		done <- err
	}()
	<-entered
	business := make(chan bool, 1)
	go func() {
		headers, _ := m.Before("business-during-write", "account-a", "model-a", nil)
		business <- headers.Get(Header) != "" && m.Status().ProbeBudget.Used == 0
	}()
	select {
	case ok := <-business:
		if !ok {
			t.Error("tentative reservation leaked or old template was lost")
		}
	case <-time.After(time.Second):
		t.Error("budget disk write blocked business requests")
	}
	close(release)
	if err := <-done; err == nil || fetches.Load() != 0 || m.Status().ProbeBudget.Used != 0 {
		t.Fatalf("failed reservation consumed quota or fetched credentials: err=%v fetches=%d", err, fetches.Load())
	}
}

func TestProbeBudgetConcurrentCallersCannotOverspend(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"probe_hourly_limit":1,"probe_accounts":["account-a","account-b"]}`)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		requests.Add(1)
		close(entered)
		<-release
		return ProbeResponse{Status: 200, Value: strings.Repeat("x", 312)}, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = m.Probe("account-a", "model-a", dummyCredential) }()
	<-entered
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Action != "busy" {
				t.Errorf("concurrent probe entered transport: %+v, %v", result, err)
			}
		}()
	}
	wg.Wait()
	close(release)
	<-done
	if result, err := m.Probe("account-b", "model-a", dummyCredential); err != nil || result.Action != "budget_wait" || requests.Load() != 1 {
		t.Fatalf("serialized callers exceeded cap: %+v, %v requests=%d", result, err, requests.Load())
	}
}

func TestProbeBudgetStorageBoundsAndClockRollbackNeverLoseCharges(t *testing.T) {
	start := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	state := diskState{Config: Config{ProbeHourlyLimit: 10000}}
	for i := range 20000 {
		state.ProbeUsage = reserveProbeUsage(state.ProbeUsage, start.Add(time.Duration(i)*100*time.Millisecond))
	}
	if len(state.ProbeUsage) > maxProbeUsageBuckets || probeBudget(state, start.Add(2000*time.Second)).Used != 20000 {
		t.Fatal("bounded accounting lost reservations")
	}
	// Add older reservations while existing future reservations remain pending.
	for i := range 4000 {
		state.ProbeUsage = reserveProbeUsage(state.ProbeUsage, start.Add(-time.Duration(i+1)*time.Second))
	}
	if err := validateProbeUsage(state.ProbeUsage); err != nil || len(state.ProbeUsage) != maxProbeUsageBuckets {
		t.Fatalf("clock rollback corrupted or expanded the ledger: %v len=%d", err, len(state.ProbeUsage))
	}
	if got := probeBudget(state, start.Add(-4000*time.Second)); got.Used != 24000 || !got.Exhausted {
		t.Fatalf("clock rollback reset charged usage: %+v", got)
	}
}
