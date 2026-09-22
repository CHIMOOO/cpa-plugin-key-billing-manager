package turnstate

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	maxAccountCookies     = 32
	maxAccountCookieBytes = 4096
)

// accountCookies is one account's upstream cookie jar. It lives in memory
// only: the cookies expire within minutes and are never written to disk.
type accountCookies struct {
	jar       cookieJar
	updatedAt time.Time
}

// CookieJarView reports one selected account's jar without its values.
type CookieJarView struct {
	Account          string    `json:"account"`
	Count            int       `json:"count"`
	UpdatedAt        time.Time `json:"updated_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	Fresh            bool      `json:"fresh"`
}

// storeCookiesLocked merges every cookie an upstream response set into the
// account's jar, whatever the response status or state length. A response
// that sets no cookie leaves the jar and its age unchanged.
func (m *Manager) storeCookiesLocked(account string, headers http.Header, now time.Time) {
	if account == "" {
		return
	}
	set, deleted := responseCookies(headers)
	if len(set) == 0 && len(deleted) == 0 {
		return
	}
	if m.cookies == nil {
		m.cookies = map[string]*accountCookies{}
	}
	entry := m.cookies[account]
	if entry == nil {
		entry = &accountCookies{}
		m.cookies[account] = entry
	}
	for _, name := range deleted {
		entry.jar.remove(name)
	}
	for _, cookie := range set {
		entry.jar.set(cookie.Name, cookie.Value)
	}
	entry.jar.limit()
	if len(set) > 0 {
		entry.updatedAt = now
	}
	if len(entry.jar.names) == 0 {
		delete(m.cookies, account)
	}
}

// freshCookiesLocked returns the account's cookies while they are within the
// cookie lifetime.
func (m *Manager) freshCookiesLocked(account string, now time.Time) string {
	entry := m.cookies[account]
	if entry == nil || len(entry.jar.names) == 0 || now.Sub(entry.updatedAt) > m.cookieTTL() {
		return ""
	}
	return entry.jar.String()
}

func (m *Manager) cookieTTL() time.Duration {
	return time.Duration(m.state.Config.CookieTTLSeconds) * time.Second
}

// injectCookiesLocked merges the fresh jar over the client's own cookies for
// a selected account and model. Observe mode never changes a request.
func (m *Manager) injectCookiesLocked(account, model string, headers http.Header, now time.Time) string {
	cfg := m.state.Config
	if !cfg.InjectCookies || cfg.DryRun || !m.inScopeLocked(account, model) {
		return ""
	}
	fresh := m.freshCookiesLocked(account, now)
	if fresh == "" {
		return ""
	}
	jar := cookieJar{}
	for k, values := range headers {
		if strings.EqualFold(k, "Cookie") {
			jar.parse(strings.Join(values, "; "))
		}
	}
	jar.parse(fresh)
	return jar.String()
}

// cookieJarsLocked lists the jars of the selected accounts and drops the rest.
func (m *Manager) cookieJarsLocked(now time.Time) []CookieJarView {
	views := []CookieJarView{}
	for account, entry := range m.cookies {
		if !contains(m.state.Config.ProbeAccounts, account) {
			delete(m.cookies, account)
			continue
		}
		expires := entry.updatedAt.Add(m.cookieTTL())
		remaining := int64(expires.Sub(now) / time.Second)
		views = append(views, CookieJarView{Account: account, Count: len(entry.jar.names), UpdatedAt: entry.updatedAt,
			ExpiresAt: expires, RemainingSeconds: max(0, remaining), Fresh: !now.After(expires)})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Account < views[j].Account })
	return views
}

// responseCookies returns the name=value pairs a response set and the names
// it deleted. Attributes are dropped.
func responseCookies(headers http.Header) ([]*http.Cookie, []string) {
	var set []*http.Cookie
	var deleted []string
	for k, values := range headers {
		if !strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		for _, line := range values {
			cookie, err := http.ParseSetCookie(line)
			if err != nil {
				continue
			}
			if cookie.Value == "" || cookie.MaxAge < 0 {
				deleted = append(deleted, cookie.Name)
				continue
			}
			set = append(set, cookie)
		}
	}
	return set, deleted
}

type cookieJar struct {
	names  []string
	values map[string]string
}

func (j *cookieJar) set(name, value string) {
	if j.values == nil {
		j.values = map[string]string{}
	}
	if _, exists := j.values[name]; !exists {
		j.names = append(j.names, name)
	}
	j.values[name] = value
}

func (j *cookieJar) remove(name string) {
	if _, exists := j.values[name]; !exists {
		return
	}
	delete(j.values, name)
	for i, existing := range j.names {
		if existing == name {
			j.names = append(j.names[:i], j.names[i+1:]...)
			break
		}
	}
}

// parse skips a malformed pair instead of discarding the whole header.
func (j *cookieJar) parse(header string) {
	for _, part := range strings.Split(header, ";") {
		cookies, err := http.ParseCookie(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		for _, cookie := range cookies {
			j.set(cookie.Name, cookie.Value)
		}
	}
}

func (j cookieJar) String() string {
	parts := make([]string, 0, len(j.names))
	for _, name := range j.names {
		parts = append(parts, name+"="+j.values[name])
	}
	return strings.Join(parts, "; ")
}

// limit keeps a small, bounded jar, dropping the newest names first.
func (j *cookieJar) limit() {
	for len(j.names) > maxAccountCookies || len(j.names) > 0 && len(j.String()) > maxAccountCookieBytes {
		j.remove(j.names[len(j.names)-1])
	}
}
