package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) (*Manager, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
	m := New()
	m.now = func() time.Time { return now }
	if err := m.Configure(filepath.Join(t.TempDir(), "billing.db")); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"enabled":true,"models":["model-a"],"probe_accounts":["account-a"],"cookie_refresh_seconds":0}`)); err != nil {
		t.Fatal(err)
	}
	return m, &now
}

func tokenAt(at time.Time) string {
	raw := make([]byte, 217)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(at.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func learn(t *testing.T, m *Manager, value string) {
	t.Helper()
	m.Before("request-a", "account-a", "model-a", nil)
	if err := m.Learn("request-a", "", "client-alias", http.Header{Header: {value}}); err != nil {
		t.Fatal(err)
	}
}

func TestModesIsolationAndResponseAttribution(t *testing.T) {
	m, now := newTestManager(t)
	token := tokenAt(*now)
	learn(t, m, token)
	if rows := m.Status().Templates; len(rows) != 1 || rows[0].Model != "model-a" {
		t.Fatalf("upstream model attribution lost: %+v", rows)
	}
	for _, pair := range [][2]string{{"account-b", "model-a"}, {"account-a", "model-b"}} {
		if headers, _ := m.Before("other", pair[0], pair[1], http.Header{Header: {strings.Repeat("x", 312)}}); len(headers) != 0 {
			t.Fatal("cross-account/model injection")
		}
	}
	if headers, _ := m.Before("no-header", "account-a", "model-a", nil); len(headers) != 0 {
		t.Fatal("replace-only injected absent header")
	}
	if headers, cleared := m.Before("degraded", "account-a", "model-a", http.Header{"x-codex-turn-state": {strings.Repeat("x", 312)}}); headers.Get(Header) != token || len(cleared) != 1 {
		t.Fatal("degraded header not replaced case-insensitively")
	}
	if err := m.Update([]byte(`{"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("always", "account-a", "model-a", nil); headers.Get(Header) != token {
		t.Fatal("always did not inject")
	}
	if headers, _ := m.Before("same", "account-a", "model-a", http.Header{Header: {token}}); len(headers) != 0 || m.Status().LastDecision.ReasonMessage.Key != "backend.turn_state_already_current" {
		t.Fatal("current template should pass unchanged")
	}
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("dry", "account-a", "model-a", nil); len(headers) != 0 || m.Status().LastDecision.Action != "dry_run" {
		t.Fatal("dry-run changed header")
	}
}

func TestRejectUnattributedInvalidExpiredAndFutureTemplates(t *testing.T) {
	for _, kind := range []string{"unattributed", "mismatch", "malformed", "wrong-version", "expired", "future", "completed"} {
		t.Run(kind, func(t *testing.T) {
			m, now := newTestManager(t)
			token := tokenAt(*now)
			account := "account-a"
			if kind != "unattributed" {
				m.Before("request", account, "model-a", nil)
			}
			switch kind {
			case "mismatch":
				account = "other-account"
			case "malformed":
				token = strings.Repeat("a", 292)
			case "wrong-version":
				raw, _ := base64.URLEncoding.DecodeString(token)
				raw[0] = 0x81
				token = base64.URLEncoding.EncodeToString(raw)
			case "expired":
				token = tokenAt(now.Add(-time.Hour))
			case "future":
				token = tokenAt(now.Add(time.Second))
			case "completed":
				m.Complete("request")
			}
			if err := m.Learn("request", account, "model-a", http.Header{Header: {token}}); err != nil {
				t.Fatal(err)
			}
			if rows := m.Status().Templates; len(rows) != 0 {
				t.Fatalf("unsafe template learned: %+v", rows)
			}
		})
	}
}

func TestPersistenceRedactionAndConfigRollback(t *testing.T) {
	m, now := newTestManager(t)
	token := tokenAt(*now)
	learn(t, m, token)
	if err := m.Update([]byte(`{"inject_mode":"always","probe_proxies":["http://dummy-user:dummy-password@proxy.invalid:8000"]}`)); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(m.Status())
	if strings.Contains(string(status), token) || strings.Contains(string(status), "dummy-password") || strings.Contains(string(status), "dummy-user") {
		t.Fatal("management status leaked template/proxy credentials")
	}
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.state.Config.ProbeProxies[0], "dummy-password") {
		t.Fatal("partial update lost proxy credentials")
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if status := loaded.Status(); len(status.Templates) != 1 || status.Config.InjectMode != "always" || !status.Config.DryRun {
		t.Fatalf("state did not survive restart: %+v", status)
	}
	info, err := os.Stat(m.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, error = %v", info, err)
	}
	// Force writes to fail after a valid load; the in-memory update must roll back.
	m.path = filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(m.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"inject_mode":"replace-only","template_length":316}`)); err == nil {
		t.Fatal("expected persistence failure")
	}
	if got := m.Status(); got.Config.InjectMode != "always" || len(got.Templates) != 1 {
		t.Fatalf("failed update changed memory: %+v", got)
	}
}

func TestTTLChangesDoNotResurrectDeletedTemplates(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(now.Add(-120*time.Second)))
	if err := m.Update([]byte(`{"ttl_seconds":60}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Update([]byte(`{"ttl_seconds":3600}`)); err != nil {
		t.Fatal(err)
	}
	loaded := New()
	loaded.now = m.now
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Status().Templates) != 0 {
		t.Fatal("expired template resurrected after reconfiguration/restart")
	}
}

func TestConfigRejectsInvalidModesAndProxies(t *testing.T) {
	m, _ := newTestManager(t)
	for _, raw := range []string{
		`{"inject_mode":"alway"}`, `{"unknwon_field":true}`, `{"ttl_seconds":7200}`,
		`{"template_length":312}`, `{"probe_proxies":["file:///etc/passwd"]}`,
		`{"probe_proxies":["http://proxy.invalid:99999"]}`, `{"probe_proxies":["http://proxy.invalid:80/path"]}`,
		`{"probe_proxies":["http://***@proxy.invalid:80"]}`, `{"enabled":true} {}`,
	} {
		if err := m.Update([]byte(raw)); err == nil {
			t.Errorf("accepted invalid configuration %s", raw)
		}
	}
}

func TestThinkingSuffixUsesActualUpstreamModelBucket(t *testing.T) {
	m, now := newTestManager(t)
	m.Before("learn-suffix", "account-a", "model-a(high)", nil)
	if err := m.Learn("learn-suffix", "", "client-alias", http.Header{Header: {tokenAt(*now)}}); err != nil {
		t.Fatal(err)
	}
	if rows := m.Status().Templates; len(rows) != 1 || rows[0].Model != "model-a" {
		t.Fatalf("thinking suffix became separate model: %+v", rows)
	}
	if err := m.Update([]byte(`{"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "(high)", "(8192)", "(-1)", "(none)", "(ultra)"} {
		if headers, _ := m.Before("use", "account-a", "model-a"+suffix, nil); headers.Get(Header) == "" {
			t.Errorf("same upstream model failed for %q", suffix)
		}
	}
	for _, name := range []string{"model-a(custom)", "model-a-version", "client-alias"} {
		if headers, _ := m.Before("other", "account-a", name, nil); len(headers) != 0 {
			t.Errorf("inferred a different model %q", name)
		}
	}
}

func TestConfigureWithDoesNotPartiallyCommit(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	before := m.path
	invalid := filepath.Join(t.TempDir(), "invalid.db")
	if err := os.WriteFile(invalid+".turn-state.json", []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	applied := false
	if err := m.ConfigureWith(invalid, func() error { applied = true; return nil }); err == nil || applied {
		t.Fatal("invalid sidecar changed companion billing state")
	}
	valid := filepath.Join(t.TempDir(), "valid.db")
	if err := m.ConfigureWith(valid, func() error { return errors.New("billing open failed") }); err == nil {
		t.Fatal("expected companion store failure")
	}
	if m.path != before || len(m.Status().Templates) != 1 {
		t.Fatal("billing failure discarded current templates")
	}
	if err := m.ConfigureWith(strings.TrimSuffix(before, ".turn-state.json"), func() error { applied = true; return nil }); err != nil || !applied {
		t.Fatal("same path skipped companion configuration")
	}
}
