package turnstate

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func cookieResponse(state string, cookies ...string) http.Header {
	headers := http.Header{"Set-Cookie": cookies}
	if state != "" {
		headers.Set(Header, state)
	}
	return headers
}

// The jar is frozen to the cookies that came with a 292: a 312's cookies
// neither replace nor delete them. Every selected request carries the fresh
// cookies with or without a template until the cookie lifetime passes.
func TestCookieJarIsFrozenTo292ResponsesAndInjectedWhileFresh(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"inject_mode":"always"}`)); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("r1", "account-a", "model-a", nil); headers != nil {
		t.Fatalf("empty jar injected %v", headers)
	}
	if err := m.Learn("r1", "account-a", "model-a", cookieResponse(strings.Repeat("d", 312), "__cf_bm=degraded; Path=/")); err != nil {
		t.Fatal(err)
	}
	if jars := m.Status().CookieJars; len(jars) != 0 {
		t.Fatalf("a 312 filled the jar: %+v", jars)
	}
	// A 292 counts even when it is not saved (here its timestamp is invalid).
	m.Before("r2", "account-a", "model-a", nil)
	if err := m.Learn("r2", "account-a", "model-a", cookieResponse(strings.Repeat("x", 292), "__cf_bm=abc; Path=/; HttpOnly", "_cfuvid=xyz; Path=/")); err != nil {
		t.Fatal(err)
	}
	headers, clear := m.Before("r3", "account-a", "model-a", http.Header{"Cookie": {"client=1; __cf_bm=old"}})
	if headers.Get("Cookie") != "client=1; __cf_bm=abc; _cfuvid=xyz" || headers.Get(Header) != "" || len(clear) != 1 || clear[0] != "Cookie" {
		t.Fatalf("fresh cookies without a template = %v %v", headers, clear)
	}
	if err := m.Learn("r3", "account-a", "model-a", cookieResponse(strings.Repeat("d", 312), "__cf_bm=later", "_cfuvid=; Max-Age=0")); err != nil {
		t.Fatal(err)
	}
	// A template and the cookies are injected together but independently.
	learn(t, m, tokenAt(*now))
	if headers, _ = m.Before("r4", "account-a", "model-a", nil); headers.Get(Header) == "" || headers.Get("Cookie") != "__cf_bm=abc; _cfuvid=xyz" {
		t.Fatalf("a 312 changed the frozen cookies: %v", headers)
	}
	// A 292's deletion removes the name without refreshing the jar's age.
	if err := m.Learn("r4", "account-a", "model-a", cookieResponse(tokenAt(*now), "_cfuvid=; Max-Age=0")); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(4 * time.Minute)
	if headers, _ = m.Before("r5", "account-a", "model-a", nil); headers.Get("Cookie") != "__cf_bm=abc" || headers.Get(Header) == "" {
		t.Fatalf("cookies within 240 s = %v", headers)
	}
	status := m.Status()
	if len(status.CookieJars) != 1 || status.CookieJars[0].Count != 1 || !status.CookieJars[0].Fresh || status.Counters.Cookies != 4 {
		t.Fatalf("cookie status = %+v %+v", status.CookieJars, status.Counters)
	}
	*now = now.Add(time.Second + time.Second)
	if headers, _ = m.Before("r6", "account-a", "model-a", nil); headers.Get("Cookie") != "" || headers.Get(Header) == "" {
		t.Fatalf("expired cookies were injected, or the one-hour template was lost: %v", headers)
	}
	if jars := m.Status().CookieJars; len(jars) != 1 || jars[0].Fresh || jars[0].RemainingSeconds != 0 {
		t.Fatalf("expired jar = %+v", jars)
	}
}

// With cookie_refresh_all on, every response refreshes the jar again.
func TestCookieRefreshAllLetsEveryResponseRefreshTheJar(t *testing.T) {
	m, _ := newTestManager(t)
	if m.Status().Config.CookieRefreshAll {
		t.Fatal("cookie_refresh_all is on by default")
	}
	if err := m.Update([]byte(`{"cookie_refresh_all":true}`)); err != nil {
		t.Fatal(err)
	}
	for i, value := range []string{strings.Repeat("d", 312), ""} {
		cookie := fmt.Sprintf("sid=%d", i)
		if err := m.Learn(fmt.Sprint("learn", i), "account-a", "model-a", cookieResponse(value, cookie)); err != nil {
			t.Fatal(err)
		}
		if headers, _ := m.Before(fmt.Sprint("r", i), "account-a", "model-a", nil); headers.Get("Cookie") != cookie {
			t.Fatalf("a %d-character response did not refresh the jar: %v", len(value), headers)
		}
	}
}

func TestCookiesStayWithinTheSelectedScope(t *testing.T) {
	m, now := newTestManager(t)
	m.Before("r1", "account-a", "model-a", nil)
	if err := m.Learn("r1", "account-a", "model-a", cookieResponse(tokenAt(*now), "sid=1")); err != nil {
		t.Fatal(err)
	}
	// Unselected accounts never fill a jar; unselected models never get one.
	if err := m.Learn("r2", "account-b", "model-a", cookieResponse(tokenAt(*now), "sid=2")); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("r3", "account-a", "model-other", nil); headers.Get("Cookie") != "" {
		t.Fatalf("out-of-scope model got cookies: %v", headers)
	}
	if len(m.Status().CookieJars) != 1 {
		t.Fatal("an unselected account filled a jar")
	}
	for _, patch := range []string{`{"dry_run":true}`, `{"dry_run":false,"inject_cookies":false}`} {
		if err := m.Update([]byte(patch)); err != nil {
			t.Fatal(err)
		}
		if headers, _ := m.Before("r4", "account-a", "model-a", nil); headers.Get("Cookie") != "" {
			t.Fatalf("%s injected cookies: %v", patch, headers)
		}
	}
}

// Only a probe response carrying a 292 fills the jar, saved or not; a 312 or
// a refusal keeps the cookies that came with the last 292.
func TestOnlyProbeResponsesWithA292FillTheCookieJar(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"probe_static_cooldown_minutes":0,"probe_account_cooldown_minutes":0}`)); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		response ProbeResponse
		want     string
	}{
		{ProbeResponse{Status: 200, Value: strings.Repeat("d", 312), SetCookies: []string{"__cf_bm=degraded; Path=/"}}, ""},
		{ProbeResponse{Status: 429, SetCookies: []string{"__cf_bm=limited"}}, ""},
		{ProbeResponse{Status: 200, Value: strings.Repeat("x", 292), SetCookies: []string{"__cf_bm=probe; Path=/"}}, "__cf_bm=probe"},
		{ProbeResponse{Status: 200, Value: strings.Repeat("d", 312), SetCookies: []string{"__cf_bm=later"}}, "__cf_bm=probe"},
		{ProbeResponse{Status: 429, SetCookies: []string{"__cf_bm=limited"}}, "__cf_bm=probe"},
	} {
		m.runProbe = func(Credential, string, string) (ProbeResponse, error) { return step.response, nil }
		if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Status != step.response.Status {
			t.Fatalf("probe %d = %+v %v", i, result, err)
		}
		if headers, _ := m.Before(fmt.Sprint("r", i), "account-a", "model-a", nil); headers.Get("Cookie") != step.want {
			t.Fatalf("probe %d: cookies = %q, want %q", i, headers.Get("Cookie"), step.want)
		}
		*now = now.Add(3 * time.Second)
	}
}

// After the first selected model harvests a 292, its bucket is probed again
// after the refresh interval; a refresh round without a 292 waits the retry
// interval before the next one.
func TestSuccessfulProbeSchedulesACookieRefreshForTheFirstModel(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30,"models":["model-a","model-b"],"probe_static_cooldown_minutes":0}`)); err != nil {
		t.Fatal(err)
	}
	var probes []string
	value := func() string { return tokenAt(*now) }
	m.runProbe = func(_ Credential, model, _ string) (ProbeResponse, error) {
		probes = append(probes, model)
		return ProbeResponse{Status: 200, Value: value()}, nil
	}
	for range 2 {
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" {
			t.Fatalf("initial harvest = %+v %v", result, err)
		}
		*now = now.Add(3 * time.Second)
	}
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "fresh" {
		t.Fatalf("both buckets should be fresh before the refresh: %+v %v", result, err)
	}
	*now = now.Add(25 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "harvested" || result.Model != "model-a" {
		t.Fatalf("cookie refresh = %+v %v", result, err)
	}
	if len(probes) != 3 || probes[2] != "model-a" {
		t.Fatalf("probe order = %v", probes)
	}
	// A refresh that meets a dry window keeps the template. With one exit the
	// round ends at once, and the next round waits the retry interval.
	*now = now.Add(31 * time.Second)
	value = func() string { return strings.Repeat("d", 312) }
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" {
		t.Fatalf("dry refresh = %+v %v", result, err)
	}
	*now = now.Add(299 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "fresh" {
		t.Fatalf("a failed refresh round retried before the retry interval: %+v %v", result, err)
	}
	*now = now.Add(time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Action != "degraded" || result.Model != "model-a" {
		t.Fatalf("a failed refresh round stopped refreshing: %+v %v", result, err)
	}
	if views := m.Status().Templates; len(views) != 2 {
		t.Fatalf("a failed refresh dropped a template: %+v", views)
	}
}

// A refresh that meets a degraded state never removes its proxy.
func TestDegradedRefreshKeepsItsProxy(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30,"probe_drop_degraded_proxies":true,"probe_min_proxies":1,"probe_static_cooldown_minutes":0,"probe_proxies":["http://first.invalid:8080","http://second.invalid:8080","http://third.invalid:8080"]}`)); err != nil {
		t.Fatal(err)
	}
	value := tokenAt(*now)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: value}, nil
	}
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "harvested" {
		t.Fatalf("harvest = %+v %v", result, err)
	}
	*now = now.Add(31 * time.Second)
	value = strings.Repeat("d", 312)
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "degraded" || result.ProxyDisposition == "removed" {
		t.Fatalf("degraded refresh = %+v %v", result, err)
	}
	if status := m.Status(); status.ProxyCounts["static"] != 3 {
		t.Fatalf("a degraded refresh removed a proxy: %+v", status.ProxyCounts)
	}
}

func TestMigrateDefaultsReturnsTheRevisionOneLifetimeToOneHour(t *testing.T) {
	state := diskState{Defaults: 1, Config: DefaultConfig()}
	state.Config.TTLSeconds, state.Config.RenewBeforeMinutes = 240, 2
	if !migrateDefaults(&state) || state.Config.TTLSeconds != 3600 || state.Config.RenewBeforeMinutes != 0 || state.Defaults != defaultsRevision {
		t.Fatalf("revision 1 lifetime not migrated: %+v", state.Config)
	}
	if migrateDefaults(&state) {
		t.Fatal("migrated twice")
	}
	custom := diskState{Defaults: 1, Config: DefaultConfig()}
	custom.Config.TTLSeconds, custom.Config.RenewBeforeMinutes = 600, 2
	if migrateDefaults(&custom) || custom.Config.TTLSeconds != 600 {
		t.Fatal("a custom lifetime was migrated")
	}
	old := diskState{Config: DefaultConfig()}
	if migrateDefaults(&old) || old.Config.TTLSeconds != 3600 || old.Defaults != defaultsRevision {
		t.Fatal("a pre-revision file changed its lifetime")
	}
	if err := validateConfig(&state.Config); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []Config{{CookieTTLSeconds: 29}, {CookieTTLSeconds: 240, CookieRefreshSeconds: -1}} {
		cfg := DefaultConfig()
		cfg.CookieTTLSeconds, cfg.CookieRefreshSeconds = patch.CookieTTLSeconds, patch.CookieRefreshSeconds
		if validateConfig(&cfg) == nil {
			t.Fatalf("invalid cookie settings accepted: %+v", patch)
		}
	}
}

func TestExitBlockedRecognizesChallengesAndRegions(t *testing.T) {
	cases := []struct {
		header http.Header
		body   string
		want   bool
	}{
		{http.Header{"Cf-Mitigated": {"challenge"}}, "", true},
		{http.Header{"Content-Type": {"text/html; charset=UTF-8"}}, "<html>", true},
		{http.Header{"Content-Type": {"application/json"}}, `{"error":{"code":"unsupported_country_region_territory"}}`, true},
		{http.Header{"Content-Type": {"application/json"}}, `{"error":{"code":"account_deactivated"}}`, false},
	}
	for _, c := range cases {
		response := &http.Response{StatusCode: 403, Header: c.header, Body: io.NopCloser(strings.NewReader(c.body))}
		if got := exitBlocked(response); got != c.want {
			t.Fatalf("exitBlocked(%v, %q) = %v", c.header, c.body, got)
		}
	}
}

// A selected account's 292 responses refresh its jar whatever the model; only
// the selected models carry the cookies.
func TestAnyModelOfASelectedAccountRefreshesTheJar(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Learn("other", "account-a", "model-other", cookieResponse(tokenAt(*now), "sid=other")); err != nil {
		t.Fatal(err)
	}
	if headers, _ := m.Before("r1", "account-a", "model-a", nil); headers.Get("Cookie") != "sid=other" {
		t.Fatalf("an out-of-scope model's response did not refresh the jar: %v", headers)
	}
	if headers, _ := m.Before("r2", "account-a", "model-other", nil); headers.Get("Cookie") != "" {
		t.Fatalf("an out-of-scope model carried cookies: %v", headers)
	}
}

// Missing buckets come before cookie refreshes, and a model that is no longer
// first stops refreshing.
func TestCookieRefreshesYieldToMissingBucketsAndFollowTheFirstModel(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30,"models":["model-a","model-b"],"probe_static_cooldown_minutes":0}`)); err != nil {
		t.Fatal(err)
	}
	m.runProbe = func(_ Credential, model, _ string) (ProbeResponse, error) {
		if model == "model-b" {
			return ProbeResponse{Status: 200, Value: strings.Repeat("d", 312)}, nil
		}
		return ProbeResponse{Status: 200, Value: tokenAt(*now)}, nil
	}
	for _, want := range []string{"model-a", "model-b"} {
		if result, err := m.Probe("", "", dummyCredential); err != nil || result.Model != want {
			t.Fatalf("initial probe = %+v %v, want %s", result, err, want)
		}
		*now = now.Add(3 * time.Second)
	}
	*now = now.Add(30 * time.Second)
	if result, err := m.Probe("", "", dummyCredential); err != nil || result.Model != "model-b" {
		t.Fatalf("a cookie refresh ran before the missing bucket: %+v %v", result, err)
	}
	if err := m.Update([]byte(`{"models":["model-b","model-a"]}`)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(3 * time.Second)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: strings.Repeat("d", 312)}, nil
	}
	if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != "fresh" {
		t.Fatalf("a model that is no longer first kept refreshing: %+v %v", result, err)
	}
}

// An unchanged refresh keeps refreshing on the same exit instead of leaving
// it reserved for the failure cooldown.
func TestUnchangedRefreshReusesItsExit(t *testing.T) {
	m, now := newTestManager(t)
	if err := m.Update([]byte(`{"cookie_refresh_seconds":30}`)); err != nil {
		t.Fatal(err)
	}
	token := tokenAt(*now)
	m.runProbe = func(Credential, string, string) (ProbeResponse, error) {
		return ProbeResponse{Status: 200, Value: token}, nil
	}
	want := []string{"harvested", "unchanged", "unchanged"}
	for i, action := range want {
		if result, err := m.Probe("account-a", "model-a", dummyCredential); err != nil || result.Action != action {
			t.Fatalf("probe %d = %+v %v, want %s", i, result, err, action)
		}
		*now = now.Add(31 * time.Second)
	}
}
