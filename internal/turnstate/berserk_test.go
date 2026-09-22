package turnstate

import (
	"sync"
	"testing"
	"time"
)

func TestInScopeCoversOnlySelectedAccountsAndModels(t *testing.T) {
	m, _ := newTestManager(t)
	if !m.InScope("account-a", "model-a(high)") {
		t.Fatal("a selected account and model with a reasoning suffix is out of scope")
	}
	if m.InScope("account-b", "model-a") || m.InScope("account-a", "model-b") {
		t.Fatal("an unselected account or model is in scope")
	}
}

func TestBerserkRaisesConcurrencyOnlyInTheLastMinutes(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	if got := m.ProbeConcurrency(); got != 1 {
		t.Fatalf("berserk off concurrency = %d", got)
	}
	if err := m.Update([]byte(`{"berserk":true,"berserk_minutes":1}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(58 * time.Minute)
	if got := m.ProbeConcurrency(); got != 1 {
		t.Fatalf("concurrency two minutes before expiry = %d", got)
	}
	*now = now.Add(90 * time.Second)
	if got := m.ProbeConcurrency(); got != BerserkConcurrency {
		t.Fatalf("concurrency in the last minute = %d", got)
	}
	*now = now.Add(time.Minute)
	if got := m.ProbeConcurrency(); got != 1 {
		t.Fatalf("an expired template kept berserk concurrency %d", got)
	}
	if err := m.Update([]byte(`{"ttl_seconds":60,"berserk":true,"berserk_minutes":1}`)); err == nil {
		t.Fatal("a berserk window as long as the lifetime was accepted")
	}
}

func TestConcurrentProbesUseSeparateExits(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://first.invalid:8080","http://second.invalid:8080","http://third.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	exits := map[string]int{}
	m.runProbe = func(_ Credential, _ string, proxy string) (ProbeResponse, error) {
		mu.Lock()
		exits[proxy]++
		mu.Unlock()
		<-release
		return ProbeResponse{Status: 502}, nil
	}
	var wg sync.WaitGroup
	results := make([]ProbeResult, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = m.ProbeConcurrently(3, nil, dummyCredential)
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		mu.Lock()
		started := len(exits)
		mu.Unlock()
		if started == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if len(exits) != 3 {
		t.Fatalf("parallel probes shared exits: %v %+v", exits, results)
	}
}

func TestBreakoutReadyNeedsTheSettingInjectionAndATemplate(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	if m.BreakoutReady("account-a", "model-a") {
		t.Fatal("breakout was ready while the setting is off")
	}
	if err := m.Update([]byte(`{"breakout_retry":true}`)); err != nil {
		t.Fatal(err)
	}
	if !m.BreakoutReady("account-a", "model-a") || m.BreakoutReady("account-a", "model-b") {
		t.Fatal("breakout readiness did not follow the template bucket")
	}
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if m.BreakoutReady("account-a", "model-a") {
		t.Fatal("breakout was ready in observe mode")
	}
}
