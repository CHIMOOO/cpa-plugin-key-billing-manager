package turnstate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func journalEvents(status JournalStatus) []string {
	events := make([]string, 0, len(status.Entries))
	for _, entry := range status.Entries {
		events = append(events, entry.Event)
	}
	return events
}

func findJournal(t *testing.T, status JournalStatus, event, source string) JournalEntry {
	t.Helper()
	for _, entry := range status.Entries {
		if entry.Event == event && (source == "" || entry.Source == source) {
			return entry
		}
	}
	t.Fatalf("no %s/%s entry in %v", event, source, journalEvents(status))
	return JournalEntry{}
}

func TestJournalIsOffByDefaultAndRecordsNothing(t *testing.T) {
	m, now := newTestManager(t)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: tokenAt(*now), SetCookies: []string{"__cf_bm=dummy-cookie"}}, nil
	}
	if _, err := m.Probe("account-a", "model-a", dummyCredential); err != nil {
		t.Fatal(err)
	}
	m.Before("r1", "account-a", "model-a", http.Header{Header: {strings.Repeat("c", 312)}})
	if p, ok := m.pending["r1"]; !ok || p.Journal != nil {
		t.Fatalf("a request captured journal facts while the journal was off: %+v", p)
	}
	if err := m.LearnResponse("r1", "", "model-a", 200, cookieResponse(strings.Repeat("d", 312), "__cf_bm=later")); err != nil {
		t.Fatal(err)
	}
	status := m.Journal(0)
	raw, _ := json.Marshal(status)
	if status.Recording || status.Count != 0 || !strings.Contains(string(raw), `"entries":[]`) || status.Limit != JournalLimit {
		t.Fatalf("journal while off = %s", raw)
	}
}

func TestJournalStartSnapshotsAndStopIsIdempotent(t *testing.T) {
	m, now := newTestManager(t)
	token := tokenAt(now.Add(-10 * time.Minute))
	m.Before("r1", "account-a", "model-a", nil)
	if err := m.Learn("r1", "", "", cookieResponse(token, "__cf_bm=dummy-cookie", "_cfuvid=dummy-uv")); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(5 * time.Second)
	status := m.SetJournalRecording(true)
	if !status.Recording || !status.Since.Equal(*now) || strings.Join(journalEvents(status), " ") != "recording_started snapshot_template snapshot_cookies" {
		t.Fatalf("start = %+v", status)
	}
	if detail := status.Entries[0].Detail; !strings.Contains(detail, "cookie_ttl=240 refresh=0 retry=300 refresh_all=false inject_cookies=true") {
		t.Fatalf("settings = %q", detail)
	}
	template := status.Entries[1]
	if template.Account != "account-a" || template.Model != "model-a" || template.Template != templateFingerprint(token) || template.Source != "response" ||
		template.TemplateAge == nil || *template.TemplateAge != 605 || !template.IssuedAt.Equal(now.Add(-605*time.Second)) {
		t.Fatalf("template snapshot = %+v", template)
	}
	jar := status.Entries[2]
	if jar.Account != "account-a" || jar.CookieCount != 2 || jar.CookieNames != "__cf_bm,_cfuvid" || jar.CookieAge == nil || *jar.CookieAge != 5 ||
		jar.Cookies != cookieFingerprint("__cf_bm=dummy-cookie; _cfuvid=dummy-uv") || len(jar.Cookies) != 8 {
		t.Fatalf("cookie snapshot = %+v", jar)
	}
	if again := m.SetJournalRecording(true); again.Count != 3 || !again.Since.Equal(status.Since) {
		t.Fatalf("starting twice changed the journal: %+v", again)
	}
	stopped := m.SetJournalRecording(false)
	if stopped.Recording || !stopped.Since.IsZero() || stopped.Count != 4 || stopped.Entries[3].Event != "recording_stopped" {
		t.Fatalf("stop = %+v", stopped)
	}
	if again := m.SetJournalRecording(false); again.Count != 4 {
		t.Fatalf("stopping twice changed the journal: %+v", again)
	}
	learn(t, m, tokenAt(*now))
	if m.Journal(0).Count != 4 {
		t.Fatal("a stopped journal kept recording")
	}
}

func TestJournalRecordsProbesCookiesRequestsAndAdminActions(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30}`)); err != nil {
		t.Fatal(err)
	}
	m.SetJournalRecording(true)
	harvested := tokenAt(*now)
	response := ProbeResponse{Status: 200, Value: harvested, SetCookies: []string{"__cf_bm=dummy-cookie; Path=/"}}
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return response, nil }
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatalf("harvest = %+v %v", result, err)
	}
	status := m.Journal(0)
	probe := findJournal(t, status, "probe", "collect")
	if probe.Account != "account-a" || probe.Model != "model-a" || probe.Action != "harvested" || probe.Status != 200 || probe.Length != 292 ||
		probe.Exit != "direct" || probe.Template != templateFingerprint(harvested) || !probe.IssuedAt.Equal(*now) || probe.Attempt != 0 {
		t.Fatalf("probe entry = %+v", probe)
	}
	updated := findJournal(t, status, "cookies_updated", "probe")
	jar := cookieFingerprint("__cf_bm=dummy-cookie")
	if updated.Length != 292 || updated.CookieNames != "__cf_bm" || updated.Cookies != jar || updated.CookieCount != 1 || updated.Model != "model-a" {
		t.Fatalf("cookie entry = %+v", updated)
	}

	// A business request records what Before injected and what came back; a
	// 312's cookies are ignored under the frozen jar.
	*now = now.Add(10 * time.Second)
	m.Before("r1", "account-a", "model-a", http.Header{Header: {strings.Repeat("c", 312)}})
	*now = now.Add(2 * time.Second)
	if err := m.LearnResponse("r1", "", "model-a", 200, cookieResponse(strings.Repeat("d", 312), "__cf_bm=later", "_cfuvid=; Max-Age=0")); err != nil {
		t.Fatal(err)
	}
	status = m.Journal(0)
	request := findJournal(t, status, "request", "")
	if request.Action != "injected" || request.Template != templateFingerprint(harvested) || request.TemplateAge == nil || *request.TemplateAge != 10 ||
		request.Cookies != jar || request.CookieAge == nil || *request.CookieAge != 10 || request.Length != 312 || request.Status != 200 ||
		request.Detail != "client_len=312" || !request.At.Equal(*now) {
		t.Fatalf("request entry = %+v", request)
	}
	ignored := findJournal(t, status, "cookies_ignored", "response")
	if ignored.Length != 312 || ignored.CookieNames != "__cf_bm" || ignored.Detail != "deleted: _cfuvid" || ignored.Account != "account-a" {
		t.Fatalf("ignored entry = %+v", ignored)
	}
	// A request without a template, carrying no cookies, still reports the jar age.
	m.Before("r2", "account-a", "model-a", nil)
	if err := m.Learn("r2", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if last := m.Journal(1).Entries[0]; last.Event != "request" || last.Action != "passed" || last.Cookies != jar || last.Length != 0 || last.Status != 0 || last.Detail != "client_len=0" {
		t.Fatalf("second request = %+v", last)
	}

	// A refresh probe is numbered within its round.
	*now = now.Add(20 * time.Second)
	response = ProbeResponse{Status: 200, Value: strings.Repeat("d", 312), SetCookies: []string{"__cf_bm=degraded"}}
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "degraded" {
		t.Fatalf("refresh = %+v %v", result, err)
	}
	status = m.Journal(2)
	if refresh := status.Entries[0]; refresh.Event != "probe" || refresh.Source != "refresh" || refresh.Attempt != 1 || refresh.Length != 312 || refresh.Template != "" {
		t.Fatalf("refresh entry = %+v", refresh)
	}
	if status.Entries[1].Event != "cookies_ignored" || status.Entries[1].Source != "probe" {
		t.Fatalf("refresh cookies = %+v", status.Entries[1])
	}

	// Business learning and administrator actions.
	*now = now.Add(time.Minute)
	learned := tokenAt(*now)
	learn(t, m, learned)
	if entry := m.Journal(1).Entries[0]; entry.Event != "template_learned" || entry.Template != templateFingerprint(learned) || !entry.IssuedAt.Equal(*now) {
		t.Fatalf("learned entry = %+v", entry)
	}
	if err := m.Discard("account-a", "model-a", templateFingerprint(learned)); err != nil {
		t.Fatal(err)
	}
	if entry := m.Journal(1).Entries[0]; entry.Event != "template_discarded" || entry.Template != templateFingerprint(learned) {
		t.Fatalf("discard entry = %+v", entry)
	}
	if err := m.Clear("", ""); err != nil {
		t.Fatal(err)
	}
	if entry := m.Journal(1).Entries[0]; entry.Event != "template_cleared" || entry.Detail != "all" {
		t.Fatalf("clear entry = %+v", entry)
	}
}

func TestJournalLimitDropsOldestAndClearKeepsRecording(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetJournalRecording(true)
	m.mu.Lock()
	for i := range JournalLimit + 4 {
		m.journalLocked(JournalEntry{At: m.now(), Event: "test", Detail: fmt.Sprint(i)})
	}
	m.mu.Unlock()
	status := m.Journal(3)
	if status.Count != JournalLimit || status.Dropped != 5 || len(status.Entries) != 3 ||
		status.Entries[0].Detail != fmt.Sprint(JournalLimit+1) || status.Entries[2].Detail != fmt.Sprint(JournalLimit+3) {
		t.Fatalf("bounded journal = %d %d %+v", status.Count, status.Dropped, status.Entries)
	}
	if all := m.Journal(0); len(all.Entries) != JournalLimit || all.Entries[0].Detail != "4" {
		t.Fatalf("full journal starts at %+v", all.Entries[0])
	}
	cleared := m.ClearJournal()
	if !cleared.Recording || cleared.Count != 0 || cleared.Dropped != 0 || cleared.Entries == nil {
		t.Fatalf("clear = %+v", cleared)
	}
	learn(t, m, tokenAt(m.now()))
	if m.Journal(0).Count == 0 {
		t.Fatal("clearing stopped the recording")
	}
	long := JournalEntry{Event: "test", Detail: strings.Repeat("é", 150)}
	m.mu.Lock()
	m.journalLocked(long)
	m.mu.Unlock()
	if detail := m.Journal(1).Entries[0].Detail; len(detail) != 200 || !strings.HasSuffix(detail, "é") {
		t.Fatalf("detail was not clipped on a rune boundary: %d bytes", len(detail))
	}
	// A data-path switch resets the journal like the observations.
	if err := m.Configure(filepath.Join(t.TempDir(), "other.db")); err != nil {
		t.Fatal(err)
	}
	if status := m.Journal(0); status.Recording || status.Count != 0 {
		t.Fatalf("journal after a path switch = %+v", status)
	}
}

func TestJournalNeverContainsTemplateCookieOrProxySecrets(t *testing.T) {
	m, now := newTestManager(t)
	const proxy = "http://dummy-user:dummy-proxy-secret@proxy.invalid:8080"
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30,"probe_proxies":["` + proxy + `"]}`)); err != nil {
		t.Fatal(err)
	}
	harvested := tokenAt(*now)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: harvested, SetCookies: []string{"__cf_bm=dummy-cookie-secret; Path=/"}}, nil
	}
	if _, err := m.Probe("account-a", "model-a", dummyCredential); err != nil {
		t.Fatal(err)
	}
	m.SetJournalRecording(true)
	m.Before("r1", "account-a", "model-a", http.Header{Header: {strings.Repeat("c", 312)}})
	if err := m.LearnResponse("r1", "", "", 200, cookieResponse(harvested, "_cfuvid=dummy-uv-secret")); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(31 * time.Second)
	if _, err := m.Probe("account-a", "model-a", dummyCredential); err != nil {
		t.Fatal(err)
	}
	status := m.SetJournalRecording(false)
	raw, _ := json.Marshal(m.Journal(0))
	for _, secret := range []string{harvested, "dummy-cookie-secret", "dummy-uv-secret", "dummy-proxy-secret", "dummy-user", strings.Repeat("c", 312)} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the journal exposes %q: %s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), `"exit":"http://***@proxy.invalid:8080"`) || len(status.Entries) < 6 {
		t.Fatalf("journal = %s", raw)
	}
}
