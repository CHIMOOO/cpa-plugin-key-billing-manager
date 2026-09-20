package turnstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readRestarted(t *testing.T, m *Manager) *Manager {
	t.Helper()
	restarted := New()
	restarted.now = m.now
	if err := restarted.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	return restarted
}

func learnFor(t *testing.T, m *Manager, id, account, model string, issued time.Time) {
	t.Helper()
	m.Before(id, account, model, nil)
	if err := m.Learn(id, account, model, http.Header{Header: {tokenAt(issued)}}); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessLearningIsNonblockingAndManagementMakesItDurable(t *testing.T) {
	m, now := newTestManager(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.writeState = func(path string, raw []byte) error {
		close(started)
		<-release
		return atomicWriteState(path, raw)
	}
	learnFor(t, m, "first", "account-a", "model-a", now.Add(-time.Minute))
	if m.Status().PendingLearnedCount != 1 || len(readRestarted(t, m).Status().Templates) != 0 {
		t.Fatal("response learning did disk I/O or lost its pending indication")
	}
	persisted := make(chan error, 1)
	go func() { persisted <- m.PersistLearned() }()
	<-started
	finished := make(chan struct{})
	go func() {
		learnFor(t, m, "newer", "account-a", "model-a", *now)
		m.Before("business", "account-a", "model-a", nil)
		m.Complete("business")
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("slow disk blocked business learning or admission")
	}
	close(release)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.PendingLearnedCount != 1 || !status.Templates[0].IssuedAt.Equal(*now) {
		t.Fatal("older snapshot overwrote newer concurrent response learning")
	}
	m.writeState = nil
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	if status := readRestarted(t, m).Status(); len(status.Templates) != 1 || !status.Templates[0].IssuedAt.Equal(*now) {
		t.Fatal("later management synchronization did not persist the newer cache")
	}
	if m.Status().PendingLearnedCount != 0 {
		t.Fatal("committed cache stayed dirty")
	}
}

func TestFailedLearningPersistenceRetainsCacheForRetry(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	m.writeState = func(string, []byte) error { return errors.New("simulated disk failure") }
	if err := m.PersistLearned(); err == nil {
		t.Fatal("failed persistence was hidden")
	}
	if status := m.Status(); status.PendingLearnedCount != 1 || status.PersistenceError == "" || len(status.Templates) != 1 {
		t.Fatal("failure lost learned cache or recovery diagnostics")
	}
	m.writeState = nil
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	if status := m.Status(); status.PendingLearnedCount != 0 || status.PersistenceError != "" {
		t.Fatal("successful retry did not clear pending/error state")
	}
}

func TestSlowClearDoesNotResurrectConcurrentLearnedTemplates(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(now.Add(-time.Minute)))
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	m.writeState = func(path string, raw []byte) error {
		close(started)
		<-release
		return atomicWriteState(path, raw)
	}
	m.Before("old-inflight", "account-a", "model-a", nil)
	m.Before("old-unrelated", "account-c", "model-a", nil)
	done := make(chan error, 1)
	go func() { done <- m.Clear("account-a", "model-a") }()
	<-started
	learnFor(t, m, "during-clear", "account-a", "model-a", *now)
	learnFor(t, m, "unrelated", "account-b", "model-a", *now)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := m.Learn("old-inflight", "account-a", "model-a", http.Header{Header: {tokenAt(*now)}}); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if len(status.Templates) != 1 || status.Templates[0].Account != "account-b" || status.PendingLearnedCount != 1 {
		t.Fatal("clear restored its own candidate or deleted unrelated learning")
	}
	m.writeState = nil
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	if rows := readRestarted(t, m).Status().Templates; len(rows) != 1 || rows[0].Account != "account-b" {
		t.Fatal("clear resurrected a stale snapshot on restart")
	}
	if err := m.Learn("old-unrelated", "account-c", "model-a", http.Header{Header: {tokenAt(*now)}}); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Templates) != 2 {
		t.Fatal("clearing one bucket invalidated an unrelated in-flight response")
	}
}

func TestSlowConfigCommitPublishesAtomicallyAndFiltersConcurrentLearning(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			m, now := newTestManager(t)
			initial := m.Status().ProxyConfigRevision
			started, release := make(chan struct{}), make(chan struct{})
			m.writeState = func(path string, raw []byte) error {
				close(started)
				<-release
				if fail {
					return errors.New("simulated write failure")
				}
				return atomicWriteState(path, raw)
			}
			done := make(chan error, 1)
			go func() { done <- m.Update([]byte(`{"template_length":316,"dry_run":true}`)) }()
			<-started
			status := m.Status()
			if status.Config.TemplateLength != 292 || status.Config.DryRun || status.ProxyConfigRevision != initial {
				t.Fatal("tentative configuration became visible before durable commit")
			}
			learn(t, m, tokenAt(*now))
			close(release)
			err := <-done
			if (err != nil) != fail {
				t.Fatal("wrong commit result", err)
			}
			status = m.Status()
			if fail && (len(status.Templates) != 1 || status.Config.TemplateLength != 292 || status.ProxyConfigRevision != initial) {
				t.Fatal("failed configuration lost concurrent response learning")
			}
			if !fail && (len(status.Templates) != 0 || status.PendingLearnedCount != 0 || status.Config.TemplateLength != 316) {
				t.Fatal("new configuration resurrected an incompatible concurrent template")
			}
		})
	}
}

func TestProbeRuntimeSnapshotsDoNotRewriteLargeProxyConfiguration(t *testing.T) {
	for _, count := range []int{500, 12000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m, now := newTestManager(t)
			proxies := make([]string, count)
			for i := range proxies {
				proxies[i] = fmt.Sprintf("socks5://dummy-session-%d:dummy-password@proxy.invalid:3000", i)
			}
			raw, _ := json.Marshal(map[string]any{"probe_proxies": proxies})
			if err := m.Update(raw); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(m.path)
			writes := 0
			m.writeState = func(path string, raw []byte) error {
				writes++
				if path != m.path+".runtime.json" || bytes.Contains(raw, []byte("dummy-password")) || len(raw) > 4000 {
					t.Error("ordinary probe rewrote or copied the proxy configuration to runtime storage")
				}
				return atomicWriteState(path, raw)
			}
			m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
				return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
			}
			result, err := m.Probe("", "", dummyCredential)
			if err != nil || result.Action != "harvested" || writes != 2 {
				t.Fatalf("probe=%+v err=%v writes=%d", result, err, writes)
			}
			after, _ := os.ReadFile(m.path)
			if !bytes.Equal(before, after) || len(readRestarted(t, m).Status().Templates) != 1 {
				t.Fatal("compact probe did not preserve configuration or durability")
			}
		})
	}
}

func TestRuntimeOverlayCannotResurrectAfterMainCheckpointABA(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	oldRuntime, _ := os.ReadFile(m.path + ".runtime.json")
	if err := m.Clear("", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"dry_run":false}`)); err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted cleanup or restoring only an obsolete overlay.
	if err := os.WriteFile(m.path+".runtime.json", oldRuntime, 0600); err != nil {
		t.Fatal(err)
	}
	if rows := readRestarted(t, m).Status().Templates; len(rows) != 0 {
		t.Fatal("stale overlay resurrected a cleared template after config ABA")
	}
}

func TestRuntimeMalformedOrIncompatibleSnapshotFailsWithoutLosingCurrentState(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	path := filepath.Join(t.TempDir(), "other")
	main, _ := os.ReadFile(m.path)
	if err := os.WriteFile(path+".turn-state.json", main, 0600); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"not-json", `{"version":9,"base":"unknown"}`} {
		if err := os.WriteFile(path+".turn-state.json.runtime.json", []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.Configure(path); err == nil || len(m.Status().Templates) != 1 {
			t.Fatal("bad runtime overlay replaced existing in-memory state")
		}
	}
}

func TestTTLIncreaseDoesNotResurrectBetweenLazyCleanupPasses(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"ttl_seconds":60}`)); err != nil {
		t.Fatal(err)
	}
	learn(t, m, tokenAt(now.Add(-59*time.Second)))
	// Avoid the once-per-minute cleanup so this covers validation independent
	// of the maintenance cadence, rather than relying on map pruning.
	m.prunedAt = *now
	*now = now.Add(2 * time.Second)
	if err := m.Update([]byte(`{"ttl_seconds":3600}`)); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Templates) != 0 || m.Status().PendingLearnedCount != 0 || len(readRestarted(t, m).Status().Templates) != 0 {
		t.Fatal("TTL increase resurrected a previously expired dirty template")
	}
}

func TestOrphanRuntimeFailsInsteadOfDroppingDurableTemplates(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.path); err != nil {
		t.Fatal(err)
	}
	if err := New().Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err == nil {
		t.Fatal("orphan runtime was silently discarded")
	}
}
