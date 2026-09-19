package turnstate

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEditableProxySaveChecksLoadedRevision(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Update([]byte(`{"probe_proxies":["http://dummy:old@first.invalid:80"]}`)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	revision := m.configRevisionTokenLocked()
	m.mu.Unlock()
	if err := m.Update([]byte(`{"dry_run":true}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"expected_revision": revision, "probe_proxies": []string{"http://dummy:new@second.invalid:80"}})
	if err := m.Update(raw); err == nil {
		t.Fatal("stale editor overwrote another saved configuration")
	}
	if !m.state.Config.DryRun || m.state.Config.ProbeProxies[0] != "http://dummy:old@first.invalid:80" {
		t.Fatal("rejected stale save changed active configuration")
	}
	m.mu.Lock()
	revision = m.configRevisionTokenLocked()
	m.mu.Unlock()
	raw, _ = json.Marshal(map[string]any{"expected_revision": revision, "probe_proxies": []string{"http://dummy:new@second.invalid:80"}})
	if err := m.Update(raw); err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(m.path)
	if err != nil || bytes.Contains(disk, []byte("expected_revision")) {
		t.Fatal("request revision was persisted as configuration", err)
	}
}

func TestChunkUploadRejectsEditorRevisionPredatingUpload(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	revision := m.configRevisionTokenLocked()
	m.mu.Unlock()
	if err := m.Update([]byte(`{"probe_proxies":["http://newer.invalid:80"]}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"expected_revision": revision, "probe_proxies": []string{}})
	id := stageTestConfig(t, m, raw)
	if err := m.CommitConfigUpload(id); err == nil || !strings.Contains(err.Error(), "Settings changed") {
		t.Fatalf("stale snapshot predating the upload was accepted: %v", err)
	}
	if len(m.state.Config.ProbeProxies) != 1 {
		t.Fatal("stale upload cleared a more recent pool")
	}
}
