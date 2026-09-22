package turnstate

import (
	"net/http"
	"strings"
)

const (
	maxTemplateCookies     = 32
	maxTemplateCookieBytes = 4096
)

// responseCookies keeps only the name=value pairs a harvesting response set.
// Attributes are dropped; deletions and oversized sets are ignored.
func responseCookies(headers http.Header) string {
	var lines []string
	for k, values := range headers {
		if strings.EqualFold(k, "Set-Cookie") {
			lines = append(lines, values...)
		}
	}
	jar := cookieJar{}
	for _, line := range lines {
		cookie, err := http.ParseSetCookie(line)
		if err != nil || cookie.Value == "" || cookie.MaxAge < 0 {
			continue
		}
		jar.set(cookie.Name, cookie.Value)
	}
	return jar.limited()
}

// injectedCookies merges the template cookies over the client's own, so a
// client cookie of another name is preserved and a stale one is replaced.
func injectedCookies(headers http.Header, t Template, cfg Config) string {
	if !cfg.InjectCookies || t.Cookies == "" {
		return ""
	}
	jar := cookieJar{}
	for k, values := range headers {
		if strings.EqualFold(k, "Cookie") {
			jar.parse(strings.Join(values, "; "))
		}
	}
	jar.parse(t.Cookies)
	return jar.String()
}

func cookieCount(cookies string) int {
	jar := cookieJar{}
	jar.parse(cookies)
	return len(jar.names)
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

// limited stores at most a small, bounded cookie set with each template.
func (j cookieJar) limited() string {
	if len(j.names) > maxTemplateCookies {
		j.names = j.names[:maxTemplateCookies]
	}
	for value := j.String(); len(j.names) > 0; value = j.String() {
		if len(value) <= maxTemplateCookieBytes {
			return value
		}
		j.names = j.names[:len(j.names)-1]
	}
	return ""
}
