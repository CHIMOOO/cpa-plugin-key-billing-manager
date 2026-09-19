package turnstate

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClearCooldownsPreservesRenewalsAndTemplates(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_accounts":["account-a","account-b"],"models":["model-a","model-b"],"probe_proxies":["http://static.invalid:8000"],"probe_proxies_rotating":["http://rotating.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	learn(t, m, tokenAt(*now))
	failed := cooldown{Until: now.Add(55 * time.Minute)}
	renew := cooldown{Until: now.Add(20 * time.Minute), RenewalBucket: key("account-a", "model-a")}
	target := proxyKey("account-a", "model-a", "http://rotating.invalid:8000", true)
	renewID := proxyKey("account-a", "model-a", "http://static.invalid:8000", false)
	other := proxyKey("account-b", "model-a", "http://static.invalid:8000", false)
	otherModel := proxyKey("account-a", "model-b", "http://static.invalid:8000", false)
	m.state.Cooldowns = map[string]cooldown{target: failed, renewID: renew, other: failed, otherModel: failed, accountKey("account-a"): failed, accountKey("account-b"): failed}
	before := m.Status()
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		t.Fatal("clear must not run probes")
		return ProbeResponse{}, nil
	}
	if count, err := m.ClearCooldowns("account-a", "model-a"); err != nil || count != 2 {
		t.Fatalf("target clear=%d %v", count, err)
	}
	if len(m.state.Cooldowns) != 4 || m.state.Cooldowns[renewID] != renew || m.state.Cooldowns[other] != failed || m.state.Cooldowns[otherModel] != failed {
		t.Fatalf("scope escaped or renewal removed: %+v", m.state.Cooldowns)
	}
	if len(m.Status().Templates) != len(before.Templates) || m.Status().Counters != before.Counters {
		t.Fatal("clear altered templates or decisions")
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if len(loaded.state.Cooldowns) != 4 {
		t.Fatal("clearing was not persisted")
	}
	if count, err := loaded.ClearCooldowns("", ""); err != nil || count != 3 {
		t.Fatalf("global clear=%d %v", count, err)
	}
	if len(loaded.state.Cooldowns) != 1 || loaded.state.Cooldowns[renewID] != renew {
		t.Fatal("global clear discarded healthy renewal")
	}
}

func TestProbeObservationFinishesBeforeReconfiguration(t *testing.T) {
	for iteration := 0; iteration < 25; iteration++ {
		m, now := newTestManager(t)
		entered, release := make(chan struct{}), make(chan struct{})
		m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
			close(entered)
			<-release
			return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
		}
		probeDone := make(chan error, 1)
		go func() { _, err := m.Probe("", "", dummyCredential); probeDone <- err }()
		<-entered
		busy, err := m.Probe("", "", dummyCredential)
		if err != nil || busy.Action != "busy" || busy.ReasonMessage.Key == "" {
			t.Fatalf("busy result=%+v %v", busy, err)
		}
		configured := make(chan error, 1)
		var previousStats ProbeStats
		var previousResult ProbeResult
		newPath := filepath.Join(t.TempDir(), "new.db")
		go func() {
			configured <- m.ConfigureWith(newPath, func() error { previousStats = m.probeStats; previousResult = m.lastProbe; return nil })
		}()
		runtime.Gosched()
		close(release)
		if err := <-probeDone; err != nil {
			t.Fatal(err)
		}
		if err := <-configured; err != nil {
			t.Fatal(err)
		}
		if previousStats.Attempts != 1 || previousStats.Harvested != 1 || previousResult.Action != "harvested" {
			t.Fatalf("reconfiguration saw incomplete prior observations: %+v %+v", previousStats, previousResult)
		}
		status := m.Status()
		if status.ProbeStats.Attempts != 0 || status.ProbeStats.Harvested != 0 || status.LastProbe.Action != "" {
			t.Fatal("old probe contaminated the new configuration")
		}
	}
}

func TestClearCooldownsRollbackAndInvalidScope(t *testing.T) {
	m, now := newTestManager(t)
	id := accountKey("account-a")
	m.state.Cooldowns[id] = cooldown{Until: now.Add(time.Hour)}
	for _, scope := range [][2]string{{"account-a", ""}, {"", "model-a"}, {"account\x00a", "model-a"}} {
		if _, err := m.ClearCooldowns(scope[0], scope[1]); err == nil {
			t.Fatal("accepted invalid clear scope")
		}
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(blocked, "state")
	if _, err := m.ClearCooldowns("", ""); err == nil || len(m.state.Cooldowns) != 1 {
		t.Fatal("failed persistence did not roll back")
	}
}

func TestDecisionAndProbeCountersExplainReloadWithoutInventingRunner(t *testing.T) {
	m, now := newTestManager(t)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.ProbeProgress.Active || status.LastProbe.Action != "harvested" || status.ProbeStats.Attempts != 1 || status.ProbeStats.Harvested != 1 || status.ProbeStats.Since.IsZero() {
		t.Fatalf("completed probe/reload status=%+v", status)
	}
	if status.LastProbe.Exit != result.Exit || status.LastProbe.ReasonMessage.Key == "" {
		t.Fatal("last result lost endpoint or message")
	}
	_, _ = m.Probe("", "", dummyCredential)
	if m.Status().ProbeStats.Attempts != 1 || m.Status().LastProbe.Action != "fresh" {
		t.Fatal("no-op refresh was counted as a network probe")
	}
	m.Before("replace", "account-a", "model-a", http.Header{Header: {strings.Repeat("x", 312)}})
	if err := m.Update([]byte(`{"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	m.Before("insert", "account-a", "model-a", nil)
	m.Before("current", "account-a", "model-a", http.Header{Header: {tokenAt(*now)}})
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	m.Before("observe", "account-a", "model-a", nil)
	if err := m.Learn("no-attribution", "", "", http.Header{Header: {tokenAt(*now)}}); err != nil {
		t.Fatal(err)
	}
	c := m.Status().Counters
	if c.Injected != 2 || c.Substituted != 1 || c.Inserted != 1 || c.Learned != 1 || c.Skipped != 1 || c.DryRun != 1 || c.Passed != 2 || c.Since.IsZero() {
		t.Fatalf("counter breakdown=%+v", c)
	}
	serialized, _ := json.Marshal(m.Status())
	if strings.Contains(string(serialized), tokenAt(*now)) {
		t.Fatal("status leaked raw template")
	}
}
