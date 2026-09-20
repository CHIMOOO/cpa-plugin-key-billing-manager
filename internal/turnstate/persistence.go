package turnstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"cpa-key-billing/internal/messages"
)

// runtimeState is a compact, atomic overlay for the immutable configuration
// snapshot. The digest binds it to the exact main file, so an older overlay
// cannot restore templates or cooldowns after a configuration replacement.
// The original version-1 main file remains readable without a migration.
type runtimeState struct {
	Version     int                 `json:"version"`
	Base        string              `json:"base"`
	Templates   map[string]Template `json:"templates"`
	Cooldowns   map[string]cooldown `json:"cooldowns"`
	ProxyCursor probeProxyCursor    `json:"proxy_cursor,omitzero"`
	ProbeUsage  []probeUsage        `json:"probe_usage,omitempty"`
}

func stateDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func cloneState(state diskState) diskState {
	// Config slices are immutable after publication. Configuration writers
	// clone them before edits; runtime commits never copy the large proxy pool.
	next := state
	next.ProbeUsage = append([]probeUsage(nil), state.ProbeUsage...)
	next.Templates = make(map[string]Template, len(state.Templates))
	for k, value := range state.Templates {
		next.Templates[k] = value
	}
	next.Cooldowns = make(map[string]cooldown, len(state.Cooldowns))
	for k, value := range state.Cooldowns {
		next.Cooldowns[k] = value
	}
	return next
}

func pruneState(state *diskState, now time.Time) {
	state.ProbeUsage = pruneProbeUsage(state.ProbeUsage, now)
	for k, value := range state.Templates {
		if k != key(value.Account, value.Model) || !usableWithConfig(value, state.Config, now) {
			delete(state.Templates, k)
		}
	}
	for k, value := range state.Cooldowns {
		if !value.Until.After(now) {
			delete(state.Cooldowns, k)
		}
	}
}

func loadRuntime(path, base string, state *diskState) error {
	raw, err := os.ReadFile(path + ".runtime.json")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return messages.Errorf("Cannot read the turn-state state file")
	}
	var overlay runtimeState
	if json.Unmarshal(raw, &overlay) != nil || overlay.Version != 1 || overlay.Base == "" {
		return messages.Errorf("Invalid turn-state state file")
	}
	if overlay.Base != base {
		return nil
	}
	if overlay.Templates == nil || overlay.Cooldowns == nil {
		return messages.Errorf("Invalid turn-state state file")
	}
	if err := os.Chmod(path+".runtime.json", 0o600); err != nil {
		return messages.Errorf("Cannot restrict turn-state state file permissions")
	}
	state.Templates, state.Cooldowns = overlay.Templates, overlay.Cooldowns
	state.ProxyCursor = overlay.ProxyCursor
	state.ProbeUsage = overlay.ProbeUsage
	return nil
}

func atomicWriteState(path string, raw []byte) error {
	if path == "" {
		return messages.Errorf("The turn-state state file has not been configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return messages.Errorf("Cannot create the turn-state state directory")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".turn-state-*")
	if err != nil {
		return messages.Errorf("Cannot create a temporary turn-state file")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return messages.Errorf("Cannot write the turn-state state")
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return messages.Errorf("Cannot replace the turn-state state file")
	}
	return nil
}

// commitStateLocked holds writerMu throughout, but releases the business mutex
// for encoding and disk I/O. No tentative configuration, deletion, or probe
// result is published until the atomic replacement succeeds. Concurrent
// response learning remains in memory and is merged after the disk commit.
// clearScope is empty for an ordinary commit, "*" for all templates, or a key.
func (m *Manager) commitStateLocked(next diskState, full bool, clearScope string) error {
	configChanged := full
	path := m.path
	base := m.baseDigest
	full = full || m.basePath != path || base == ""
	write := m.writeState
	previousConfig := m.state.Config
	if write == nil {
		write = atomicWriteState
	}
	m.mu.Unlock()
	var raw []byte
	var err error
	var configDigest string
	writePath := path
	if full {
		// Preserve the next surviving entry across configuration edits and
		// automatic removal. The pools are immutable snapshots; reconciliation
		// and hashing stay outside the mutex used by business requests.
		next.ProxyCursor = reconcileProbeProxyCursor(previousConfig, next.Config, next.ProxyCursor)
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err == nil {
			next.CheckpointID = hex.EncodeToString(nonce[:])
		}
		if err == nil {
			raw, err = json.Marshal(next)
		}
	} else {
		writePath += ".runtime.json"
		raw, err = json.Marshal(runtimeState{Version: 1, Base: base, Templates: next.Templates, Cooldowns: next.Cooldowns, ProxyCursor: next.ProxyCursor, ProbeUsage: next.ProbeUsage})
	}
	if err == nil {
		err = write(writePath, raw)
	}
	if err == nil && full {
		base = stateDigest(raw)
		configRaw, _ := json.Marshal(next.Config)
		configDigest = stateDigest(configRaw)
	}
	m.mu.Lock()
	if err != nil {
		m.persistenceError = "Cannot save the template"
		return err
	}
	m.persistenceError = ""
	m.runtimeDirty = false
	m.basePath, m.baseDigest = path, base
	if full {
		m.revisionToken, m.revisionTokenAt = configDigest, m.configRevision
		if configChanged {
			m.revisionTokenAt++
		}
	}
	// Remove only candidates included in this commit. A later response must
	// remain dirty, and a clear/configuration boundary must not resurrect an
	// older candidate that arrived while the disk write was in progress.
	for k, learned := range m.dirtyTemplates {
		if clearScope == "*" || clearScope == k || !usableWithConfig(learned, next.Config, m.now()) || !usableWithConfig(learned, m.state.Config, m.now()) {
			delete(m.dirtyTemplates, k)
			continue
		}
		persisted, exists := next.Templates[k]
		if exists && !learned.IssuedAt.After(persisted.IssuedAt) {
			delete(m.dirtyTemplates, k)
			continue
		}
		next.Templates[k] = learned
	}
	m.state = next
	if clearScope != "" {
		m.templateEpoch++
		if clearScope == "*" {
			m.allTemplateEpoch = m.templateEpoch
			m.clearedTemplates = map[string]uint64{}
		} else {
			m.clearedTemplates[clearScope] = m.templateEpoch
		}
	}
	return nil
}

// PersistLearned is called by an authenticated management synchronization, not
// a response hook. New business templates are an in-memory cache until that
// synchronization or another management/probe commit. Failed writes preserve
// the cache for the next call. No timer, goroutine, or response delay is added.
func (m *Manager) PersistLearned() error {
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.dirtyTemplates) == 0 && !m.runtimeDirty {
		return nil
	}
	return m.commitStateLocked(cloneState(m.state), false, "")
}
