package turnstate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDiscardExactFingerprintPersistsAndBlocksRelearning(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"ttl_seconds":60}`)); err != nil {
		t.Fatal(err)
	}
	issued := now.Add(-30 * time.Second)
	value := tokenAt(issued)
	learn(t, m, value)
	view := m.Status().Templates[0]
	if view.Fingerprint != templateFingerprint(value) || len(view.Fingerprint) != 64 {
		t.Fatal("missing exact fingerprint", view)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), value) {
		t.Fatal("template view exposed raw State")
	}
	if err := m.Discard("account-a", "model-a", view.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Templates) != 0 || len(m.state.Discarded) != 1 {
		t.Fatal("discard was not committed")
	}
	entry := m.state.Discarded[discardKey("account-a", "model-a", view.Fingerprint)]
	if !entry.ExpiresAt.Equal(issued.Add(time.Hour)) {
		t.Fatal("discard lifetime followed short configured TTL", entry)
	}
	m = readRestarted(t, m)
	if err := m.Update([]byte(`{"ttl_seconds":3600}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	learn(t, m, value)
	if len(m.Status().Templates) != 0 || m.Status().LastDecision.Action != "skip" {
		t.Fatal("longer TTL revived a discarded template")
	}
	if err := m.Clear("", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ClearCooldowns("", ""); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	learn(t, m, value)
	if len(m.Status().Templates) != 0 || len(m.state.Discarded) != 1 {
		t.Fatal("clear controls erased discard protection")
	}
	// The exact same opaque value remains independent across account/model buckets.
	learnFor(t, m, "other-account", "account-b", "model-a", issued)
	learnFor(t, m, "other-model", "account-a", "model-b", issued)
	if len(m.Status().Templates) != 2 {
		t.Fatal("discard escaped its exact bucket")
	}
	// Different values at the same issuance second are not timestamp-banned.
	bytes, _ := base64.URLEncoding.DecodeString(value)
	bytes[len(bytes)-1] = 1
	alternative := base64.URLEncoding.EncodeToString(bytes)
	learn(t, m, alternative)
	if got := m.state.Templates[key("account-a", "model-a")].Value; got != alternative {
		t.Fatal("discard rejected a distinct value with the same timestamp")
	}
	*now = entry.ExpiresAt
	m.Status()
	if len(m.state.Discarded) != 0 {
		t.Fatal("expired tombstone was not pruned")
	}
}

func TestDiscardRejectsStaleAndInvalidSelectionsWithoutWriting(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(now.Add(-time.Minute)))
	old := m.Status().Templates[0].Fingerprint
	learn(t, m, tokenAt(*now))
	writes := 0
	m.writeState = func(string, []byte) error { writes++; return errors.New("unexpected write") }
	if err := m.Discard("account-a", "model-a", old); !errors.Is(err, ErrTemplateChanged) {
		t.Fatal("stale click removed replacement", err)
	}
	for _, input := range [][3]string{{"", "", old}, {"account-a", "", old}, {"account-a", "model-a", "short"}, {"account-a", "model-a", strings.ToUpper(old)}} {
		if err := m.Discard(input[0], input[1], input[2]); !errors.Is(err, ErrInvalidDiscard) {
			t.Fatal("invalid discard accepted", input, err)
		}
	}
	current := m.Status().Templates[0]
	*now = now.Add(time.Hour)
	if err := m.Discard(current.Account, current.Model, current.Fingerprint); !errors.Is(err, ErrTemplateChanged) {
		t.Fatal("expired template discarded", err)
	}
	if writes != 0 {
		t.Fatal("rejected discard wrote storage")
	}
}

func TestDiscardPersistenceFailurePreservesTemplateAndPendingEpoch(t *testing.T) {
	m, now := newTestManager(t)
	value := tokenAt(*now)
	learn(t, m, value)
	before := m.Status().Templates[0]
	epoch := m.templateEpoch
	m.state.Cooldowns["renewal"] = cooldown{Until: now.Add(55 * time.Minute), RenewalBucket: key("account-a", "model-a")}
	m.writeState = func(string, []byte) error { return errors.New("dummy disk failure") }
	if err := m.Discard(before.Account, before.Model, before.Fingerprint); err == nil {
		t.Fatal("discard reported success without persistence")
	}
	if got := m.Status().Templates; len(got) != 1 || got[0].Fingerprint != before.Fingerprint || len(m.state.Discarded) != 0 || m.templateEpoch != epoch || m.state.Cooldowns["renewal"].RenewalBucket == "" {
		t.Fatal("failed discard changed active state")
	}
	if m.Status().PendingLearnedCount != 1 {
		t.Fatal("failed discard dropped unpersisted template")
	}
}

func TestDiscardReleasesOnlyItsSuccessfulRenewalWaits(t *testing.T) {
	m, now := newTestManager(t)
	value := tokenAt(*now)
	learn(t, m, value)
	bucket := key("account-a", "model-a")
	m.state.Cooldowns = map[string]cooldown{
		"target-renewal": {Until: now.Add(55 * time.Minute), RenewalBucket: bucket},
		"other-renewal":  {Until: now.Add(55 * time.Minute), RenewalBucket: key("account-b", "model-a")},
		"failure":        {Until: now.Add(10 * time.Minute), Attempts: 4},
	}
	m.state.ProbeUsage = []probeUsage{{At: *now, Count: 2}}
	if err := m.Discard("account-a", "model-a", templateFingerprint(value)); err != nil {
		t.Fatal(err)
	}
	if _, exists := m.state.Cooldowns["target-renewal"]; exists {
		t.Fatal("discard retained a successful-template wait for the removed value")
	}
	if len(m.state.Cooldowns) != 2 || m.state.Cooldowns["failure"].Attempts != 4 || m.Status().ProbeBudget.Used != 2 {
		t.Fatal("discard reset another bucket, failure cooldown, or hourly usage")
	}
}

func TestDiscardDoesNotBlockBusinessAndKeepsNewerConcurrentLearning(t *testing.T) {
	m, now := newTestManager(t)
	old := tokenAt(now.Add(-time.Minute))
	learn(t, m, old)
	fingerprint := m.Status().Templates[0].Fingerprint
	m.Before("pending-before-discard", "account-a", "model-a", nil)
	started, release := make(chan struct{}), make(chan struct{})
	m.writeState = func(path string, raw []byte) error { close(started); <-release; return atomicWriteState(path, raw) }
	done := make(chan error, 1)
	go func() { done <- m.Discard("account-a", "model-a", fingerprint) }()
	<-started
	learned := make(chan struct{})
	go func() {
		learn(t, m, tokenAt(*now))
		m.Before("business-during-discard", "account-a", "model-a", nil)
		m.Complete("business-during-discard")
		close(learned)
	}()
	select {
	case <-learned:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("discard disk write blocked business hooks")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := m.Status().Templates; len(got) != 1 || got[0].Fingerprint == fingerprint || m.Status().PendingLearnedCount != 1 {
		t.Fatal("discard erased a newer concurrent response", got)
	}
	// An old in-flight attribution cannot repopulate a post-discard bucket.
	*now = now.Add(time.Second)
	if err := m.Learn("pending-before-discard", "", "", http.Header{Header: {tokenAt(*now)}}); err != nil {
		t.Fatal(err)
	}
	if got := m.state.Templates[key("account-a", "model-a")]; !got.IssuedAt.Equal(now.Add(-time.Second)) {
		t.Fatal("pending pre-discard response crossed epoch")
	}
	m.writeState = nil
	if err := m.PersistLearned(); err != nil {
		t.Fatal(err)
	}
	m = readRestarted(t, m)
	learn(t, m, old)
	if len(m.Status().Templates) != 1 || m.Status().Templates[0].Fingerprint == fingerprint {
		t.Fatal("old discarded value returned after restart")
	}
}

func TestProbeRejectsDiscardedValueWithoutPruningProxy(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://first.invalid:8000","http://second.invalid:8000"],"probe_drop_failed_proxies":true,"probe_drop_degraded_proxies":true,"probe_min_proxies":1}`)); err != nil {
		t.Fatal(err)
	}
	value := tokenAt(*now)
	learn(t, m, value)
	if err := m.Discard("account-a", "model-a", templateFingerprint(value)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: value}, nil
	}
	result, err := m.Probe("", "", dummyCredential)
	if err != nil || result.Action != "discarded" || result.ProxyDisposition != "" || len(m.Status().Templates) != 0 || m.Status().ProxyCounts["static"] != 2 || m.Status().ProbeStats.Failed != 0 || m.Status().ProbeStats.Degraded != 0 {
		t.Fatal("discarded result was mistaken for quality/proxy failure", result, err)
	}
	if got := m.state.Cooldowns[proxyKey("account-a", "model-a", "http://first.invalid:8000", false)].Until; !got.Equal(now.Add(55 * time.Minute)) {
		t.Fatal("discarded probe lost retry reservation", got)
	}
	*now = now.Add(3 * time.Second)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" || result.ProxyIndex != 2 {
		t.Fatal("new template did not replace discarded value", result, err)
	}
}

func TestDiscardTombstonesSurviveNeighboringMainAndRuntimeSnapshots(t *testing.T) {
	for _, variant := range []string{"stale-runtime", "matching-runtime-without-discard", "stale-main"} {
		t.Run(variant, func(t *testing.T) {
			m, now := newTestManager(t)
			value := tokenAt(*now)
			learn(t, m, value)
			if err := m.PersistLearned(); err != nil {
				t.Fatal(err)
			}
			oldMain, err := os.ReadFile(m.path)
			if err != nil {
				t.Fatal(err)
			}
			oldRuntime, err := os.ReadFile(m.path + ".runtime.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Discard("account-a", "model-a", templateFingerprint(value)); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "matching-runtime-without-discard":
				var overlay runtimeState
				if err := json.Unmarshal(oldRuntime, &overlay); err != nil {
					t.Fatal(err)
				}
				overlay.Base = m.baseDigest
				raw, _ := json.Marshal(overlay)
				if err := os.WriteFile(m.path+".runtime.json", raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "stale-main":
				// A post-discard runtime mutation carries the same exact deny list.
				if err := m.Clear("", ""); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(m.path, oldMain, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m = readRestarted(t, m)
			learn(t, m, value)
			if len(m.Status().Templates) != 0 || len(m.state.Discarded) != 1 {
				t.Fatal("neighboring snapshot revived discarded template")
			}
		})
	}
}

func TestDiscardValidationAndCapacityAreBounded(t *testing.T) {
	m, now := newTestManager(t)
	value := tokenAt(*now)
	learn(t, m, value)
	m.state.Discarded = map[string]discardedTemplate{}
	for i := 0; i < maxDiscardedTemplates; i++ {
		entry := discardedTemplate{Account: fmt.Sprintf("account-%d", i), Model: "model-a", Fingerprint: templateFingerprint(fmt.Sprint(i)), IssuedAt: *now, ExpiresAt: now.Add(time.Hour)}
		m.state.Discarded[discardKey(entry.Account, entry.Model, entry.Fingerprint)] = entry
	}
	if err := m.Discard("account-a", "model-a", templateFingerprint(value)); !errors.Is(err, ErrDiscardCapacity) {
		t.Fatal("discard silently evicted live protection", err)
	}
	if len(m.Status().Templates) != 1 {
		t.Fatal("capacity rejection removed template")
	}
	if err := validateDiscarded(m.state.Discarded); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []discardedTemplate{
		{Account: "account-a", Model: "model-a", Fingerprint: "bad", IssuedAt: *now, ExpiresAt: now.Add(time.Hour)},
		{Account: "account-a", Model: "model-a", Fingerprint: templateFingerprint(value), IssuedAt: *now, ExpiresAt: now.Add(2 * time.Hour)},
	} {
		if err := validateDiscarded(map[string]discardedTemplate{discardKey(invalid.Account, invalid.Model, invalid.Fingerprint): invalid}); err == nil {
			t.Fatal("invalid tombstone accepted", invalid)
		}
	}
}
