package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// callerScopeSalt must stay byte-identical to CLIProxyAPI's
// sdk/cliproxy/session.CallerScope. The host derives caller_scope with this
// exact prefix and places it in interceptor metadata; this plugin derives the
// same value when the admin UI sends the configured key list. Changing it
// silently detaches those synchronized keys from request accounting, so a
// fixed host-generated vector covers it.
const callerScopeSalt = "cli-proxy-api:caller-scope:v1\x00"

const UnknownKeyPreview = "unknown"

// Blank principals are not attributable and therefore have no scope.
func CallerScope(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(callerScopeSalt + value))
	return hex.EncodeToString(sum[:])
}

// PreviewKey masks a plaintext key for display and persistence.
func PreviewKey(key string) string {
	characters := []rune(strings.TrimSpace(key))
	if len(characters) < 4 {
		return strings.Repeat("*", len(characters))
	}
	// Reveal no more than half of a short key, up to eight characters per end.
	edge := min(8, len(characters)/4)
	return string(characters[:edge]) + "…" + string(characters[len(characters)-edge:])
}

func freeID(name, prefix string, taken func(string) bool) string {
	// Names without Latin letters use numbered IDs rather than numeric fragments.
	if base := slugify(name); strings.ContainsAny(base, "abcdefghijklmnopqrstuvwxyz") {
		if !taken(base) {
			return base
		}
		for i := 2; i < 1000; i++ {
			if candidate := fmt.Sprintf("%s-%d", base, i); !taken(candidate) {
				return candidate
			}
		}
	}
	for i := 1; ; i++ {
		if candidate := fmt.Sprintf("%s-%d", prefix, i); !taken(candidate) {
			return candidate
		}
	}
}

func slugify(value string) string {
	var builder strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && builder.Len() > 0 {
				builder.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}
