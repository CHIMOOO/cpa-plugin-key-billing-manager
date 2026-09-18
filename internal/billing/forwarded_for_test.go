package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeForwardedForBlock(t *testing.T) {
	manyKeywords := func(count int) []string {
		keywords := make([]string, count)
		for i := range keywords {
			keywords[i] = fmt.Sprintf("model-%03d", i)
		}
		return keywords
	}
	for _, test := range []struct {
		name    string
		input   ForwardedForBlock
		want    ForwardedForBlock
		invalid bool
	}{
		{name: "zero value", want: ForwardedForBlock{ModelKeywords: []string{}, Message: DefaultForwardedForBlockMessage}},
		{
			name:  "trims and removes case-insensitive repeats in order",
			input: ForwardedForBlock{Enabled: true, ModelKeywords: []string{" gpt ", "", "Claude", "GPT", "  ", "claude", "gemini"}, Message: "  自定义提示  "},
			want:  ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt", "Claude", "gemini"}, Message: "自定义提示"},
		},
		{
			name:  "disabled keeps keywords",
			input: ForwardedForBlock{ModelKeywords: []string{"gpt"}, Message: "   "},
			want:  ForwardedForBlock{ModelKeywords: []string{"gpt"}, Message: DefaultForwardedForBlockMessage},
		},
		{name: "enabled without keywords", input: ForwardedForBlock{Enabled: true, ModelKeywords: []string{"", "  "}}, invalid: true},
		{name: "enabled with nil keywords", input: ForwardedForBlock{Enabled: true}, invalid: true},
		{
			name:  "longest keyword",
			input: ForwardedForBlock{ModelKeywords: []string{strings.Repeat("a", maxForwardedForKeywordBytes)}},
			want:  ForwardedForBlock{ModelKeywords: []string{strings.Repeat("a", maxForwardedForKeywordBytes)}, Message: DefaultForwardedForBlockMessage},
		},
		{name: "keyword too long", input: ForwardedForBlock{ModelKeywords: []string{strings.Repeat("a", maxForwardedForKeywordBytes+1)}}, invalid: true},
		{
			name:  "most keywords",
			input: ForwardedForBlock{Enabled: true, ModelKeywords: append(manyKeywords(maxForwardedForKeywords), "MODEL-000")},
			want:  ForwardedForBlock{Enabled: true, ModelKeywords: manyKeywords(maxForwardedForKeywords), Message: DefaultForwardedForBlockMessage},
		},
		{name: "too many keywords", input: ForwardedForBlock{Enabled: true, ModelKeywords: manyKeywords(maxForwardedForKeywords + 1)}, invalid: true},
		{
			name:  "longest message",
			input: ForwardedForBlock{Message: strings.Repeat("m", maxForwardedForMessageBytes)},
			want:  ForwardedForBlock{ModelKeywords: []string{}, Message: strings.Repeat("m", maxForwardedForMessageBytes)},
		},
		{name: "message too long", input: ForwardedForBlock{Message: strings.Repeat("提", maxForwardedForMessageBytes/3+1)}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeForwardedForBlock(test.input)
			if test.invalid {
				if KindOf(err) != KindInvalid {
					t.Fatalf("error = %v, want invalid", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalized = %+v, want %+v", got, test.want)
			}
		})
	}
	raw, err := json.Marshal(DefaultForwardedForBlock())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"enabled":false,"model_keywords":[],"message":"` + DefaultForwardedForBlockMessage + `"}`; string(raw) != want {
		t.Fatalf("default JSON = %s, want %s", raw, want)
	}
	if _, err := NormalizeForwardedForBlock(ForwardedForBlock{Enabled: true}); err == nil || err.Error() != "启用 X-Forwarded-For 拦截时至少填写一个模型关键词" {
		t.Fatalf("enabled without keywords error = %v", err)
	}
}

func TestForwardedForBlockMatchesAndTogglesImmediately(t *testing.T) {
	store := newStore(t)
	if got := store.ForwardedForBlock(); !reflect.DeepEqual(got, DefaultForwardedForBlock()) {
		t.Fatalf("defaults = %+v", got)
	}
	if _, blocked := store.MatchForwardedForBlock("gpt-5.5"); blocked {
		t.Fatal("disabled default blocked a model")
	}
	saved, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: []string{"GPT", " claude-3 ", "gpt"}})
	if err != nil {
		t.Fatal(err)
	}
	want := ForwardedForBlock{Enabled: true, ModelKeywords: []string{"GPT", "claude-3"}, Message: DefaultForwardedForBlockMessage}
	if !reflect.DeepEqual(saved, want) || !reflect.DeepEqual(store.ForwardedForBlock(), want) {
		t.Fatalf("saved = %+v, stored = %+v", saved, store.ForwardedForBlock())
	}
	for _, test := range []struct {
		models  []string
		blocked bool
		model   string
		keyword string
	}{
		{models: []string{"gpt-5.5"}, blocked: true, model: "gpt-5.5", keyword: "GPT"},
		{models: []string{"openai/ChatGPT-4o"}, blocked: true, model: "openai/ChatGPT-4o", keyword: "GPT"},
		{models: []string{"Claude-3-Opus"}, blocked: true, model: "Claude-3-Opus", keyword: "claude-3"},
		{models: []string{"gemini-3.7-flash", " gpt-auto "}, blocked: true, model: "gpt-auto", keyword: "GPT"},
		{models: []string{"claude-3-haiku", "gpt-5.5"}, blocked: true, model: "claude-3-haiku", keyword: "claude-3"},
		{models: []string{"gemini-3.7-flash", "claude-opus-5"}},
		{models: []string{"", "   "}},
		{},
	} {
		match, blocked := store.MatchForwardedForBlock(test.models...)
		if blocked != test.blocked || match.Model != test.model || match.Keyword != test.keyword {
			t.Fatalf("Match(%q) = %+v, %v", test.models, match, blocked)
		}
		if blocked && match.Message != DefaultForwardedForBlockMessage {
			t.Fatalf("message = %q", match.Message)
		}
	}

	if _, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}, Message: "禁止混用"}); err != nil {
		t.Fatal(err)
	}
	if match, blocked := store.MatchForwardedForBlock("GPT-5.5"); !blocked || match.Message != "禁止混用" {
		t.Fatalf("custom message = %+v, %v", match, blocked)
	}
	if _, err := store.SetForwardedForBlock(ForwardedForBlock{ModelKeywords: []string{"gpt"}, Message: "禁止混用"}); err != nil {
		t.Fatal(err)
	}
	if _, blocked := store.MatchForwardedForBlock("gpt-5.5"); blocked {
		t.Fatal("disabled rule still blocks")
	}
	if got := store.ForwardedForBlock(); got.Enabled || !reflect.DeepEqual(got.ModelKeywords, []string{"gpt"}) {
		t.Fatalf("disabling lost keywords: %+v", got)
	}

	if _, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: []string{}}); KindOf(err) != KindInvalid {
		t.Fatalf("enabled without keywords: %v", err)
	}
	if got := store.ForwardedForBlock(); got.Enabled || got.Message != "禁止混用" {
		t.Fatalf("invalid save changed settings: %+v", got)
	}
}

func TestForwardedForBlockKeywordsDoNotAliasState(t *testing.T) {
	store := newStore(t)
	input := []string{"gpt", "claude"}
	saved, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: input})
	if err != nil {
		t.Fatal(err)
	}
	input[0] = "changed-input"
	saved.ModelKeywords[1] = "changed-result"
	read := store.ForwardedForBlock()
	read.ModelKeywords[0] = "changed-read"
	want := []string{"gpt", "claude"}
	if got := store.ForwardedForBlock().ModelKeywords; !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords aliased caller slices: %v", got)
	}

	// A failed edit works on a copy, so its changes never reach live state.
	if _, err := editConfiguration(store, func(state *State) (struct{}, Changes, error) {
		state.ForwardedForBlock.ModelKeywords[0] = "changed-by-failed-edit"
		return struct{}{}, Changes{}, errors.New("dummy failure")
	}); err == nil {
		t.Fatal("expected edit failure")
	}
	if got := store.ForwardedForBlock().ModelKeywords; !reflect.DeepEqual(got, want) {
		t.Fatalf("failed edit changed live keywords: %v", got)
	}
	if match, blocked := store.MatchForwardedForBlock("gpt-5.5"); !blocked || match.Keyword != "gpt" {
		t.Fatalf("live rule changed: %+v, %v", match, blocked)
	}
}

func TestForwardedForBlockPersistenceFailureKeepsPolicy(t *testing.T) {
	store, repo := newStoreWithRepository(t)
	if _, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}}); err != nil {
		t.Fatal(err)
	}
	if !repo.state.ForwardedForBlock.Enabled {
		t.Fatal("settings were not handed to the repository")
	}
	before := store.ForwardedForBlock()
	repo.fail = errors.New("disk full")
	if _, err := store.SetForwardedForBlock(ForwardedForBlock{}); err == nil {
		t.Fatal("write failure not returned")
	}
	if got := store.ForwardedForBlock(); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed write changed active settings: %+v", got)
	}
	if _, blocked := store.MatchForwardedForBlock("gpt-5.5"); !blocked {
		t.Fatal("failed write disabled the rule")
	}
}

func TestForwardedForChangesAreTracked(t *testing.T) {
	changes := Changes{ForwardedForBlock: true}
	if changes.empty() {
		t.Fatal("setting change treated as empty")
	}
	if merged := (Changes{Keys: []string{"scope"}}).merge(changes); !merged.ForwardedForBlock || len(merged.Keys) != 1 {
		t.Fatalf("merge lost the setting change: %+v", merged)
	}
	if merged := changes.merge(Changes{Plans: true}); !merged.ForwardedForBlock || !merged.Plans {
		t.Fatalf("merge lost the setting change: %+v", merged)
	}
}

func forwardedForPluginLogs(t *testing.T, store *Store) []PluginLog {
	t.Helper()
	var kept []PluginLog
	for _, event := range mustPluginLogs(t, store) {
		if strings.HasPrefix(event.Message, "X-Forwarded-For 拦截：") {
			kept = append(kept, event)
		}
	}
	return kept
}

func TestForwardedForBlockIsReportedOncePerKeywordWindow(t *testing.T) {
	store := failingStore(t)
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	gpt := ForwardedForMatch{Model: "gpt-5.5", Keyword: "gpt", Message: "禁止混用"}
	for range 3 {
		store.ReportForwardedForBlock("scope-a", "/v1/chat/completions", gpt)
	}
	events := forwardedForPluginLogs(t, store)
	if len(events) != 1 || events[0].Level != PluginLogInfo {
		t.Fatalf("events = %+v, want one information entry", events)
	}
	for _, want := range []string{"Alice · sk-tes…0001", " → /v1/chat/completions", "，模型 gpt-5.5", "，命中关键词 gpt"} {
		if !strings.Contains(events[0].Message, want) {
			t.Fatalf("message = %q, want it to contain %q", events[0].Message, want)
		}
	}
	if strings.Contains(events[0].Message, "scope-a") || strings.Contains(events[0].Message, "禁止混用") {
		t.Fatalf("message = %q, want the key described and the client message left out", events[0].Message)
	}

	// Mixing models under one keyword is one refusal episode, not a row per request.
	for i := range 20 {
		model := "gpt-4o"
		if i%2 == 0 {
			model = "gpt-5-mini"
		}
		store.ReportForwardedForBlock("scope-a", "/v1/messages", ForwardedForMatch{Model: model, Keyword: "gpt"})
	}
	if events := forwardedForPluginLogs(t, store); len(events) != 1 {
		t.Fatalf("events = %+v, want alternating models reported once", events)
	}
	store.ReportForwardedForBlock("scope-a", "/v1/messages", ForwardedForMatch{Model: "claude-opus-5", Keyword: "claude"})
	store.ReportForwardedForBlock("scope-b", "/v1/messages", ForwardedForMatch{Model: "gpt-4o", Keyword: "gpt"})
	if events := forwardedForPluginLogs(t, store); len(events) != 3 {
		t.Fatalf("events = %+v, want another keyword and another key reported", events)
	}
	// A quota refusal keeps its own record of what it reported.
	store.ReportQuotaBlock("scope-a", "/v1/chat/completions", Decision{PlanID: "plan", QuotaView: QuotaView{Blocked: true}})
	if events := admissionPluginLogs(t, store); len(events) != 1 {
		t.Fatalf("quota events = %+v", events)
	}

	now = now.Add(forwardedForReportInterval - time.Second)
	store.ReportForwardedForBlock("scope-a", "/v1/chat/completions", gpt)
	if events := forwardedForPluginLogs(t, store); len(events) != 3 {
		t.Fatalf("events = %+v, want no report inside the window", events)
	}
	now = now.Add(time.Second)
	store.ReportForwardedForBlock("scope-a", "/v1/chat/completions", gpt)
	if events := forwardedForPluginLogs(t, store); len(events) != 4 {
		t.Fatalf("events = %+v, want a later episode reported again", events)
	}
	// A clock stepping backwards must not silence reports until it catches up.
	now = now.Add(-time.Hour)
	store.ReportForwardedForBlock("scope-a", "/v1/chat/completions", gpt)
	if events := forwardedForPluginLogs(t, store); len(events) != 5 {
		t.Fatalf("events = %+v, want a report after the clock moved back", events)
	}

	if _, err := store.SetForwardedForBlock(ForwardedForBlock{Enabled: true, ModelKeywords: []string{"gpt"}}); err != nil {
		t.Fatal(err)
	}
	store.ReportForwardedForBlock("scope-a", "/v1/chat/completions", ForwardedForMatch{Model: "gpt-4o", Keyword: "gpt"})
	if events := forwardedForPluginLogs(t, store); len(events) != 6 {
		t.Fatalf("events = %+v, want saving the settings to report again", events)
	}

	store.ReportForwardedForBlock("", "", gpt)
	events = forwardedForPluginLogs(t, store)
	if len(events) != 7 || events[0].Message != "X-Forwarded-For 拦截：未识别的 API Key，模型 gpt-5.5，命中关键词 gpt" {
		t.Fatalf("unknown scope events = %+v", events)
	}
}

func TestForwardedForReportsStayBounded(t *testing.T) {
	var reports forwardedForReports
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	at := func(t time.Time) func() time.Time { return func() time.Time { return t } }
	for i := range maxForwardedForReports {
		if !reports.due(fmt.Sprintf("scope-%d\x00gpt", i), at(now)) {
			t.Fatalf("report %d was not due", i)
		}
	}
	if reports.due("overflow\x00gpt", at(now.Add(time.Minute))) {
		t.Fatal("a full set of recent reports accepted another key")
	}
	if len(reports.last) != maxForwardedForReports {
		t.Fatalf("len = %d", len(reports.last))
	}
	if !reports.due("overflow\x00gpt", at(now.Add(forwardedForReportInterval))) {
		t.Fatal("expired reports were not pruned for a new key")
	}
	if len(reports.last) != 1 {
		t.Fatalf("len after pruning = %d, want 1", len(reports.last))
	}
	// A slightly earlier reading is not a new window.
	if reports.due("overflow\x00gpt", at(now.Add(forwardedForReportInterval-time.Second))) {
		t.Fatal("a small step back reported again")
	}
}

func TestForwardedForBlockConcurrentRefusalsReportOnce(t *testing.T) {
	store := failingStore(t)
	gpt := ForwardedForMatch{Model: "gpt-5.5", Keyword: "gpt"}
	for burst := range 50 {
		store.forwardedForReports.reset()
		before := len(forwardedForPluginLogs(t, store))
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				store.ReportForwardedForBlock("scope-a", "/v1/responses", gpt)
			}()
		}
		close(start)
		wg.Wait()
		if got := len(forwardedForPluginLogs(t, store)) - before; got != 1 {
			t.Fatalf("burst %d wrote %d entries, want 1", burst, got)
		}
	}
}
