package billing

import "testing"

// TestCallerScopeMatchesHostVectors pins the hash to values produced by
// CLIProxyAPI's own sdk/cliproxy/session.CallerScope. Enforcement reads the
// host-computed caller_scope while Key synchronization derives it locally. If
// these disagree, a plan bound in the UI never matches traffic. Regenerate the
// vectors with the host implementation.
func TestCallerScopeMatchesHostVectors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "api key",
			input: "sk-test-key-0001",
			want:  "6df3f70e71486751d90152257b4a86ead883a8861692be5221363c2077dd540f",
		},
		{
			name:  "short principal",
			input: "hello",
			want:  "a7db0244a3a1996a99259fd55d99ca80f57f2d41f96bb93942cdd40f2fef4cd1",
		},
		{
			name:  "empty is not attributable",
			input: "",
			want:  "",
		},
		{
			name:  "surrounding whitespace is trimmed like the host does",
			input: "  sk-test-key-0001\t",
			want:  "6df3f70e71486751d90152257b4a86ead883a8861692be5221363c2077dd540f",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CallerScope(test.input); got != test.want {
				t.Fatalf("CallerScope(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestPreviewKeyDoesNotLeakShortKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "one character", input: "a", want: "*"},
		{name: "two characters", input: "ab", want: "**"},
		{name: "three characters", input: "abc", want: "***"},
		{name: "four characters", input: "abcd", want: "a…d"},
		{name: "short key", input: "sk-123", want: "s…3"},
		{name: "eight characters", input: "12345678", want: "12…78"},
		{name: "twelve characters", input: "123456789012", want: "123…012"},
		{name: "sixteen characters", input: "sk-test-key-0001", want: "sk-t…0001"},
		{name: "eight visible per end", input: "12345678abcdefghijklmnop87654321", want: "12345678…87654321"},
		{name: "long key", input: "sk-dummy-abcdefghijklmnopqrstuvwxyz-12345678", want: "sk-dummy…12345678"},
		{name: "unicode characters", input: "甲乙丙丁", want: "甲…丁"},
		{name: "unicode short key", input: "甲乙丙", want: "***"},
		{name: "surrounding whitespace", input: " \tabcd\n", want: "a…d"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PreviewKey(test.input); got != test.want {
				t.Fatalf("PreviewKey(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}
