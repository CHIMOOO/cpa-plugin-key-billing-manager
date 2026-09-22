package turnstate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// JournalLimit bounds the diagnostic journal; the oldest entries are dropped.
const JournalLimit = 20000

const (
	journalStatusTail  = 100 // Entries returned when recording starts, stops or is cleared.
	journalDetailLimit = 200
	journalNamesLimit  = 512
)

// JournalEntry is one diagnostic event. It never holds a turn-state value, a
// cookie value or proxy credentials: templates and cookie sets appear only as
// fingerprints, exits in masked form and cookies by name.
type JournalEntry struct {
	At          time.Time `json:"at"`
	Event       string    `json:"event"`
	Account     string    `json:"account,omitempty"`
	Model       string    `json:"model,omitempty"`
	Source      string    `json:"source,omitempty"`
	Action      string    `json:"action,omitempty"`
	Exit        string    `json:"exit,omitempty"`     // Masked, never credentials.
	Status      int       `json:"status,omitempty"`   // Upstream HTTP status, when known.
	Length      int       `json:"length,omitempty"`   // Turn-state length in that response.
	Attempt     int       `json:"attempt,omitempty"`  // Refresh round attempt number, from 1.
	Template    string    `json:"template,omitempty"` // Template fingerprint, never the value.
	IssuedAt    time.Time `json:"issued_at,omitzero"`
	TemplateAge *int64    `json:"template_age_seconds,omitempty"`
	Cookies     string    `json:"cookies,omitempty"` // Cookie set fingerprint.
	CookieCount int       `json:"cookie_count,omitempty"`
	CookieAge   *int64    `json:"cookie_age_seconds,omitempty"`
	CookieNames string    `json:"cookie_names,omitempty"` // Sorted names, never values.
	Detail      string    `json:"detail,omitempty"`       // Short English text.
}

// JournalStatus reports the journal. Entries are the latest ones, oldest first.
type JournalStatus struct {
	Recording bool           `json:"recording"`
	Since     time.Time      `json:"since,omitzero"` // When the current recording started.
	Count     int            `json:"count"`
	Dropped   uint64         `json:"dropped"` // Oldest entries dropped at the limit.
	Limit     int            `json:"limit"`
	Entries   []JournalEntry `json:"entries"`
}

// journalState is volatile diagnostics like the observations: off at process
// start, reset by a data-path switch and never persisted. Its entries form a
// ring of at most JournalLimit.
type journalState struct {
	recording bool
	since     time.Time
	entries   []JournalEntry
	head      int // The oldest entry once the ring is full.
	dropped   uint64
}

// requestFacts is what Before did to a business request while the journal
// recorded, kept with its attribution until the response is learned.
type requestFacts struct {
	action       string
	template     string
	templateAge  *int64
	cookies      string
	cookieAge    *int64
	clientLength int
}

// SetJournalRecording starts or stops recording; repeating the current state
// changes nothing. Starting records the settings and a snapshot of the
// selected accounts' templates and cookie jars. It returns the latest 100
// entries.
func (m *Manager) SetJournalRecording(on bool) JournalStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	switch {
	case on && !m.journal.recording:
		m.journal.recording, m.journal.since = true, now.UTC()
		m.journalLocked(JournalEntry{At: now, Event: "recording_started", Detail: journalSettings(m.state.Config)})
		m.journalSnapshotLocked(now)
	case !on && m.journal.recording:
		m.journalLocked(JournalEntry{At: now, Event: "recording_stopped"})
		m.journal.recording, m.journal.since = false, time.Time{}
	}
	return m.journalStatusLocked(journalStatusTail)
}

// ClearJournal drops every entry and keeps recording if it was. It returns
// the (empty) status with the same shape as Journal.
func (m *Manager) ClearJournal() JournalStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal.entries, m.journal.head, m.journal.dropped = nil, 0, 0
	return m.journalStatusLocked(journalStatusTail)
}

// Journal returns the latest tail entries, or every entry when tail <= 0.
func (m *Manager) Journal(tail int) JournalStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.journalStatusLocked(tail)
}

func (m *Manager) journalStatusLocked(tail int) JournalStatus {
	j := m.journal
	count := len(j.entries)
	if tail <= 0 || tail > count {
		tail = count
	}
	entries := make([]JournalEntry, 0, tail)
	for i := count - tail; i < count; i++ {
		entries = append(entries, j.entries[(j.head+i)%count])
	}
	return JournalStatus{Recording: j.recording, Since: j.since, Count: count, Dropped: j.dropped, Limit: JournalLimit, Entries: entries}
}

// journalLocked appends one entry while recording. Callers that must compute
// anything for an entry check m.journal.recording first, so every hook costs
// a boolean check while the journal is off.
func (m *Manager) journalLocked(entry JournalEntry) {
	j := &m.journal
	if !j.recording {
		return
	}
	entry.At = entry.At.UTC()
	entry.Detail = clipText(entry.Detail, journalDetailLimit)
	if len(j.entries) < JournalLimit {
		j.entries = append(j.entries, entry)
		return
	}
	j.entries[j.head] = entry
	j.head = (j.head + 1) % JournalLimit
	j.dropped++
}

// journalSnapshotLocked records the usable templates of the selected accounts
// and models and the selected accounts' cookie jars.
func (m *Manager) journalSnapshotLocked(now time.Time) {
	var templates []Template
	for _, t := range m.state.Templates {
		if m.inScopeLocked(t.Account, t.Model) && m.usableLocked(t, now) {
			templates = append(templates, t)
		}
	}
	sort.Slice(templates, func(i, j int) bool {
		return key(templates[i].Account, templates[i].Model) < key(templates[j].Account, templates[j].Model)
	})
	for _, t := range templates {
		m.journalLocked(JournalEntry{At: now, Event: "snapshot_template", Account: t.Account, Model: t.Model, Source: t.Source,
			Exit: maskSavedExit(t.Exit), Template: templateFingerprint(t.Value), IssuedAt: t.IssuedAt, TemplateAge: ageSeconds(now, t.IssuedAt)})
	}
	accounts := make([]string, 0, len(m.cookies))
	for account := range m.cookies {
		if contains(m.state.Config.ProbeAccounts, account) {
			accounts = append(accounts, account)
		}
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		entry := m.cookies[account]
		m.journalLocked(JournalEntry{At: now, Event: "snapshot_cookies", Account: account, Cookies: cookieFingerprint(entry.jar.String()),
			CookieCount: len(entry.jar.names), CookieAge: ageSeconds(now, entry.updatedAt), CookieNames: joinNames(entry.jar.names)})
	}
}

// captureRequestLocked keeps what Before did to a selected account and
// model's request with its pending attribution. A request without a pending
// entry (no request ID) is not journaled.
func (m *Manager) captureRequestLocked(requestID, account, model, action string, injectedCookies bool, clientLength int, now time.Time) {
	p, ok := m.pending[requestID]
	if !ok || !m.inScopeLocked(account, model) {
		return
	}
	facts := &requestFacts{action: action, clientLength: clientLength}
	if t, exists := m.state.Templates[key(account, model)]; exists && action != "no_template" {
		facts.template, facts.templateAge = templateFingerprint(t.Value), ageSeconds(now, t.IssuedAt)
	}
	// The jar's age is kept even when it is too old to be injected.
	if jar := m.cookies[account]; jar != nil {
		facts.cookieAge = ageSeconds(now, jar.updatedAt)
		if injectedCookies {
			facts.cookies = cookieFingerprint(jar.jar.String())
		}
	}
	p.Journal = facts
	m.pending[requestID] = p
}

// journalRequestLocked records a business request when its response arrives:
// what Before did, and the response's status and turn-state length.
func (m *Manager) journalRequestLocked(p pending, status, length int, now time.Time) {
	f := p.Journal
	m.journalLocked(JournalEntry{At: now, Event: "request", Account: p.Account, Model: p.Model, Action: f.action,
		Template: f.template, TemplateAge: f.templateAge, Cookies: f.cookies, CookieAge: f.cookieAge,
		Status: status, Length: length, Detail: fmt.Sprintf("client_len=%d", f.clientLength)})
}

// journalProbeLocked records a finished probe, including one whose result
// could not be saved.
func (m *Manager) journalProbeLocked(c probeCandidate, response ProbeResponse, attempt int, result ProbeResult, err error, now time.Time) {
	if !m.journal.recording {
		return
	}
	source := "collect"
	if c.refresh {
		source = "refresh"
	} else if c.renewal {
		source = "renewal"
	}
	entry := JournalEntry{At: now, Event: "probe", Account: c.account, Model: c.model, Source: source, Action: result.Action,
		Exit: maskProxy(c.proxy), Status: response.Status, Length: len(response.Value), Attempt: attempt, Detail: result.Reason}
	if err != nil {
		entry.Action, entry.Detail = "error", err.Error()
	}
	if len(response.Value) == m.state.Config.TemplateLength {
		entry.Template = templateFingerprint(response.Value)
		if issued, ok := issuedAt(response.Value); ok {
			entry.IssuedAt = issued
		}
	}
	m.journalLocked(entry)
}

func journalSettings(cfg Config) string {
	return fmt.Sprintf("cookie_ttl=%d refresh=%d retry=%d refresh_all=%t inject_cookies=%t inject_mode=%s dry_run=%t",
		cfg.CookieTTLSeconds, cfg.CookieRefreshSeconds, cfg.CookieRetrySeconds, cfg.CookieRefreshAll, cfg.InjectCookies, cfg.InjectMode, cfg.DryRun)
}

func ageSeconds(now, since time.Time) *int64 {
	age := int64(now.Sub(since) / time.Second)
	return &age
}

// cookieFingerprint identifies a cookie set by 8 hex digits of its SHA-256.
func cookieFingerprint(jar string) string {
	if jar == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(jar))
	return hex.EncodeToString(digest[:4])
}

func cookieNames(cookies []*http.Cookie) string {
	names := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		names = append(names, cookie.Name)
	}
	return joinNames(names)
}

func deletedDetail(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "deleted: " + joinNames(names)
}

// joinNames lists cookie names sorted and without duplicates, never values.
func joinNames(names []string) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(names)))
	return clipText(strings.Join(sorted, ","), journalNamesLimit)
}

// clipText shortens text to at most limit bytes without splitting a rune.
func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
