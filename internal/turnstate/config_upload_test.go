package turnstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stageTestConfig(t *testing.T, m *Manager, raw []byte) string {
	t.Helper()
	upload, err := m.BeginConfigUpload(len(raw))
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(raw); offset += ConfigChunkBytes {
		end := min(offset+ConfigChunkBytes, len(raw))
		status, err := m.AppendConfigUpload(upload.ID, offset, raw[offset:end])
		if err != nil || status.Received != end {
			t.Fatalf("chunk offset=%d received=%d error=%v", offset, status.Received, err)
		}
	}
	return upload.ID
}

func TestConfigUploadLargePoolsCommitTogetherAndPersist(t *testing.T) {
	m, _ := newTestManager(t)
	const count = 6000
	static, rotating := make([]string, count), make([]string, count)
	for i := range static {
		static[i] = fmt.Sprintf("http://dummy-user:dummy-password-%s@static-%d.example:8000", strings.Repeat("a", 70), i)
		rotating[i] = fmt.Sprintf("socks5h://dummy-user:dummy-password-%s@rotating-%d.example:1080", strings.Repeat("b", 70), i)
	}
	raw, err := json.Marshal(map[string]any{"inject_mode": "always", "models": []string{"unicode-模型"},
		"probe_proxies": static, "probe_proxies_rotating": rotating})
	if err != nil || len(raw) < 1<<20 {
		t.Fatalf("fixture must exceed a typical 1 MiB reverse-proxy limit: bytes=%d err=%v", len(raw), err)
	}
	id := stageTestConfig(t, m, raw)
	if status := m.Status(); status.Config.InjectMode != "replace-only" || status.ProxyCounts["static"] != 0 || status.ProxyCounts["rotating"] != 0 {
		t.Fatal("uncommitted upload changed active settings")
	}
	if err := m.CommitConfigUpload(id); err != nil {
		t.Fatal(err)
	}
	loaded := New()
	if err := loaded.Configure(strings.TrimSuffix(m.path, ".turn-state.json")); err != nil {
		t.Fatal(err)
	}
	if got := loaded.state.Config; len(got.ProbeProxies) != count || len(got.ProbeProxiesRotating) != count || got.Models[0] != "unicode-模型" || got.ProbeProxies[4095] != static[4095] || got.ProbeProxiesRotating[5999] != rotating[5999] {
		t.Fatal("committed configuration was truncated or did not survive restart")
	}
	status := loaded.Status()
	if status.ProxyCounts["static"] != count || status.ProxyCounts["rotating"] != count || len(status.Config.ProbeProxies) != 0 || len(status.Config.ProbeProxiesRotating) != 0 {
		t.Fatal("large pools must return counts without copying proxy URLs to status")
	}
	view, _ := json.Marshal(status)
	if len(view) > 2048 || bytes.Contains(view, []byte("dummy-password")) {
		t.Fatal("large pools made status unbounded or disclosed credentials")
	}
	if len(m.uploads) != 0 {
		t.Fatal("committed staging data was retained")
	}
}

func TestConfigUploadRejectsPartialInvalidAndStaleChanges(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"enabled":false,"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := m.BeginConfigUpload(20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendConfigUpload(partial.ID, 0, []byte(`{"enabled":`)); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitConfigUpload(partial.ID); err == nil {
		t.Fatal("incomplete upload committed")
	}
	m.CancelConfigUpload(partial.ID)
	for _, raw := range []string{
		`{"enabled":true,"probe_proxies":["not-a-url"]}`,
		`{"enabled":true,"probe_proxies_rotating":["http://***@proxy.example:80"]}`,
		`{"enabled":true,"unexpected":true}`,
		`{"enabled":true} {}`,
	} {
		id := stageTestConfig(t, m, []byte(raw))
		if err := m.CommitConfigUpload(id); err == nil {
			t.Fatal("invalid settings committed")
		}
		m.CancelConfigUpload(id)
	}
	after, _ := os.ReadFile(m.path)
	if !bytes.Equal(before, after) || m.state.Config.Enabled || m.state.Config.InjectMode != "always" {
		t.Fatal("failed upload changed active or persisted settings")
	}
	id := stageTestConfig(t, m, []byte(`{"enabled":true}`))
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitConfigUpload(id); err == nil || m.state.Config.Enabled || !m.state.Config.DryRun {
		t.Fatal("stale upload overwrote a concurrent direct save")
	}
}

func TestConfigUploadChunkOrderingBoundsExpiryAndCancellation(t *testing.T) {
	m, now := newTestManager(t)
	for _, size := range []int{-1, 0, MaxConfigBytes + 1} {
		if _, err := m.BeginConfigUpload(size); err == nil {
			t.Fatal("invalid size accepted")
		}
	}
	upload, _ := m.BeginConfigUpload(12)
	if _, err := m.AppendConfigUpload(upload.ID, 1, []byte("abc")); err == nil {
		t.Fatal("out-of-order chunk accepted")
	}
	if _, err := m.AppendConfigUpload(upload.ID, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if got, err := m.AppendConfigUpload(upload.ID, 0, []byte("abcd")); err != nil || got.Received != 4 {
		t.Fatal("exact retry was not idempotent")
	}
	for _, chunk := range []struct {
		offset int
		data   []byte
	}{{0, []byte("abce")}, {-1, []byte("x")}, {4, nil}, {4, []byte("123456789")}, {4, make([]byte, ConfigChunkBytes+1)}} {
		if _, err := m.AppendConfigUpload(upload.ID, chunk.offset, chunk.data); err == nil {
			t.Fatal("invalid chunk accepted")
		}
	}
	second, _ := m.BeginConfigUpload(2)
	if _, err := m.BeginConfigUpload(2); err == nil {
		t.Fatal("unbounded concurrent uploads accepted")
	}
	m.CancelConfigUpload(second.ID)
	if _, err := m.BeginConfigUpload(2); err != nil {
		t.Fatal("cancellation did not free a staging slot")
	}
	*now = now.Add(configUploadTTL)
	m.Status() // Cleanup is synchronous and does not require another upload.
	if len(m.uploads) != 0 {
		t.Fatal("expired upload data was retained after a host callback")
	}
	if err := m.CommitConfigUpload(upload.ID); err == nil {
		t.Fatal("expired upload committed")
	}
	id := stageTestConfig(t, m, []byte(`{}`))
	if err := m.Configure(filepath.Join(t.TempDir(), "other.db")); err != nil {
		t.Fatal(err)
	}
	if err := m.CommitConfigUpload(id); err == nil {
		t.Fatal("upload crossed a database reconfiguration")
	}
}

func TestConfigUploadPersistenceFailureRollsBackState(t *testing.T) {
	m, now := newTestManager(t)
	learn(t, m, tokenAt(*now))
	m.state.Cooldowns["expired"] = cooldown{Until: now.Add(-time.Second)}
	id := stageTestConfig(t, m, []byte(`{"enabled":false,"template_length":316}`))
	m.path = t.TempDir() // Renaming a file onto a directory must fail.
	if err := m.CommitConfigUpload(id); err == nil {
		t.Fatal("expected a persistence failure")
	}
	if !m.state.Config.Enabled || m.state.Config.TemplateLength != 292 || len(m.state.Templates) != 1 || len(m.state.Cooldowns) != 1 {
		t.Fatal("failed write changed configuration, templates, or cooldowns")
	}
}

func TestConfigPoolAndByteLimits(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update(make([]byte, MaxConfigBytes+1)); err == nil {
		t.Fatal("oversized direct save accepted")
	}
	for _, field := range []string{"probe_proxies", "probe_proxies_rotating"} {
		values := make([]string, MaxProxyPoolEntries+1)
		for i := range values {
			values[i] = fmt.Sprintf("http://proxy-%d.example:80", i)
		}
		raw, _ := json.Marshal(map[string]any{field: values})
		if err := m.Update(raw); err == nil {
			t.Fatal("oversized proxy pool accepted")
		}
		raw, _ = json.Marshal(map[string]any{field: values[:MaxProxyPoolEntries]})
		if err := m.Update(raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigByteLimitIncludesPoolsOmittedByPartialSave(t *testing.T) {
	m, _ := newTestManager(t)
	values := make([]string, 3000)
	for i := range values {
		values[i] = fmt.Sprintf("http://dummy-%s@proxy-%d.example:80", strings.Repeat("p", 3000), i)
	}
	static, _ := json.Marshal(map[string]any{"probe_proxies": values})
	rotating, _ := json.Marshal(map[string]any{"probe_proxies_rotating": values})
	if len(static) >= MaxConfigBytes || len(rotating) >= MaxConfigBytes {
		t.Fatal("each partial fixture must fit the request limit")
	}
	if err := m.Update(static); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(rotating); err == nil {
		t.Fatal("partial saves exceeded the combined configuration byte limit")
	}
	if len(m.state.Config.ProbeProxies) != len(values) || len(m.state.Config.ProbeProxiesRotating) != 0 {
		t.Fatal("rejected partial save changed the pools")
	}
}
