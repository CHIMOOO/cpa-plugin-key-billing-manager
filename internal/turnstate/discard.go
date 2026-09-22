package turnstate

import (
	"encoding/hex"
	"strings"
	"time"

	"cpa-key-billing/internal/messages"
)

const maxDiscardedTemplates = 4096

var (
	ErrInvalidDiscard  = messages.Errorf("Account, model, and a complete template fingerprint are required to discard a template")
	ErrTemplateChanged = messages.Errorf("The template changed or expired; refresh the table before discarding it")
	ErrDiscardCapacity = messages.Errorf("Too many discarded templates are still valid; wait for an older discard to expire")
)

// A discard is scoped to one exact account/model/value, not a model-wide
// timestamp barrier. Only digests are persisted, never an additional raw State.
// Its lifetime is the upstream maximum, independently of an operator's TTL.
type discardedTemplate struct {
	Account     string    `json:"account"`
	Model       string    `json:"model"`
	Fingerprint string    `json:"fingerprint"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func templateFingerprint(value string) string { return stateDigest([]byte(value)) }

func discardKey(account, model, fingerprint string) string {
	return stateDigest([]byte(key(account, model) + "\x00" + fingerprint))
}

func validTemplateFingerprint(fingerprint string) bool {
	if len(fingerprint) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(fingerprint)
	return err == nil && len(decoded) == 32 && fingerprint == strings.ToLower(fingerprint)
}

func templateDiscarded(state diskState, account, model, value string, now time.Time) bool {
	if len(state.Discarded) == 0 {
		return false
	}
	entry, exists := state.Discarded[discardKey(account, model, templateFingerprint(value))]
	return exists && entry.ExpiresAt.After(now)
}

func validateDiscarded(entries map[string]discardedTemplate) error {
	if len(entries) > maxDiscardedTemplates {
		return messages.Errorf("Invalid turn-state state file")
	}
	for id, entry := range entries {
		if !validBucket(entry.Account, entry.Model) || ModelName(entry.Model) != entry.Model ||
			!validTemplateFingerprint(entry.Fingerprint) || id != discardKey(entry.Account, entry.Model, entry.Fingerprint) ||
			entry.IssuedAt.IsZero() || !entry.ExpiresAt.Equal(entry.IssuedAt.Add(time.Hour)) {
			return messages.Errorf("Invalid turn-state state file")
		}
	}
	return nil
}

func pruneDiscarded(state *diskState, now time.Time) {
	for id, entry := range state.Discarded {
		if !entry.ExpiresAt.After(now) {
			delete(state.Discarded, id)
		}
	}
}

// Discarded values must never return when a runtime overlay or a neighboring
// configuration checkpoint is older than the template cache it accompanies.
// Unlike templates/cooldowns, tombstones merge monotonically until expiry.
func mergeDiscarded(state *diskState, entries map[string]discardedTemplate, now time.Time) (bool, error) {
	if err := validateDiscarded(entries); err != nil {
		return false, err
	}
	pruneDiscarded(state, now)
	changed := false
	for id, entry := range entries {
		if !entry.ExpiresAt.After(now) {
			continue
		}
		if existing, ok := state.Discarded[id]; ok {
			if existing.Account != entry.Account || existing.Model != entry.Model || existing.Fingerprint != entry.Fingerprint || !existing.IssuedAt.Equal(entry.IssuedAt) || !existing.ExpiresAt.Equal(entry.ExpiresAt) {
				return false, messages.Errorf("Invalid turn-state state file")
			}
			continue
		}
		if len(state.Discarded) >= maxDiscardedTemplates {
			return false, ErrDiscardCapacity
		}
		if state.Discarded == nil {
			state.Discarded = map[string]discardedTemplate{}
		}
		state.Discarded[id] = entry
		changed = true
	}
	return changed, nil
}

// Discard compares the exact visible fingerprint before writing. It publishes
// removal only after its tombstone is durable; business hooks continue using
// their current in-memory state while the disk write runs.
func (m *Manager) Discard(account, model, fingerprint string) error {
	account, model = strings.TrimSpace(account), ModelName(model)
	if !validBucket(account, model) || !validTemplateFingerprint(fingerprint) {
		return ErrInvalidDiscard
	}
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	m.writerMu.Lock()
	defer m.writerMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	bucket := key(account, model)
	template, exists := m.state.Templates[bucket]
	if !exists || !m.usableLocked(template, now) || templateFingerprint(template.Value) != fingerprint {
		return ErrTemplateChanged
	}
	next := cloneState(m.state)
	pruneState(&next, now)
	if len(next.Discarded) >= maxDiscardedTemplates {
		return ErrDiscardCapacity
	}
	if next.Discarded == nil {
		next.Discarded = map[string]discardedTemplate{}
	}
	next.Discarded[discardKey(account, model, fingerprint)] = discardedTemplate{
		Account: account, Model: model, Fingerprint: fingerprint, IssuedAt: template.IssuedAt, ExpiresAt: template.IssuedAt.Add(time.Hour),
	}
	delete(next.Templates, bucket)
	// Successful-template waits must not postpone replacing the discarded
	// value. Failure cooldowns and the global hourly ledger remain untouched.
	for id, rest := range next.Cooldowns {
		if rest.RenewalBucket == bucket {
			delete(next.Cooldowns, id)
		}
	}
	// A full checkpoint leaves the tombstone in the main file even if an old
	// runtime overlay is restored later. Exact tombstone filtering in the
	// commit merger preserves a newer, different concurrent learned value.
	if err := m.commitStateLocked(next, true, ""); err != nil {
		return err
	}
	m.templateEpoch++
	m.clearedTemplates[bucket] = m.templateEpoch
	if m.journal.recording {
		m.journalLocked(JournalEntry{At: now, Event: "template_discarded", Account: account, Model: model, Template: fingerprint, IssuedAt: template.IssuedAt})
	}
	return nil
}
