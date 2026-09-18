package billing

import (
	"strings"
	"sync"
	"time"
)

// DefaultForwardedForBlockMessage is what a refused client reads when the
// operator has not written a message of their own.
const DefaultForwardedForBlockMessage = "当前禁止模型混用，请联系相关管理员了解详情。"

const (
	maxForwardedForKeywords     = 100
	maxForwardedForKeywordBytes = 512
	maxForwardedForMessageBytes = 1024
)

// ForwardedForBlock refuses requests for models whose name contains one of the
// keywords when the request carries an X-Forwarded-For header. It is off by
// default, and every request reads the current settings, so a saved change
// applies to the very next request.
type ForwardedForBlock struct {
	Enabled       bool     `json:"enabled"`
	ModelKeywords []string `json:"model_keywords"`
	Message       string   `json:"message"`
}

// ForwardedForMatch names the model and keyword that refused a request, with
// the message the client is to read.
type ForwardedForMatch struct {
	Model   string
	Keyword string
	Message string
}

func DefaultForwardedForBlock() ForwardedForBlock {
	return ForwardedForBlock{ModelKeywords: []string{}, Message: DefaultForwardedForBlockMessage}
}

// NormalizeForwardedForBlock trims the keywords, drops empty ones and repeats
// that differ only in case, keeping the first spelling in its original order.
// An empty message stands for the default one.
func NormalizeForwardedForBlock(settings ForwardedForBlock) (ForwardedForBlock, error) {
	keywords := make([]string, 0, min(len(settings.ModelKeywords), maxForwardedForKeywords))
	seen := make(map[string]struct{}, len(settings.ModelKeywords))
	for _, keyword := range settings.ModelKeywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		if len(keyword) > maxForwardedForKeywordBytes {
			return ForwardedForBlock{}, invalidf("模型关键词不能超过 %d 字节", maxForwardedForKeywordBytes)
		}
		folded := strings.ToLower(keyword)
		if _, exists := seen[folded]; exists {
			continue
		}
		seen[folded] = struct{}{}
		keywords = append(keywords, keyword)
		if len(keywords) > maxForwardedForKeywords {
			return ForwardedForBlock{}, invalidf("模型关键词不能超过 %d 个", maxForwardedForKeywords)
		}
	}
	if settings.Enabled && len(keywords) == 0 {
		return ForwardedForBlock{}, invalidf("启用 X-Forwarded-For 拦截时至少填写一个模型关键词")
	}
	message := strings.TrimSpace(settings.Message)
	if len(message) > maxForwardedForMessageBytes {
		return ForwardedForBlock{}, invalidf("提示语不能超过 %d 字节", maxForwardedForMessageBytes)
	}
	if message == "" {
		message = DefaultForwardedForBlockMessage
	}
	return ForwardedForBlock{Enabled: settings.Enabled, ModelKeywords: keywords, Message: message}, nil
}

func cloneForwardedForBlock(settings ForwardedForBlock) ForwardedForBlock {
	settings.ModelKeywords = append([]string{}, settings.ModelKeywords...)
	return settings
}

func (s *Store) ForwardedForBlock() ForwardedForBlock {
	var settings ForwardedForBlock
	s.read(func(state *State) { settings = cloneForwardedForBlock(state.ForwardedForBlock) })
	return settings
}

// SetForwardedForBlock saves the normalized settings and returns them as saved.
// Saving starts the block reports over, so the next refusal is logged again.
func (s *Store) SetForwardedForBlock(settings ForwardedForBlock) (ForwardedForBlock, error) {
	normalized, err := NormalizeForwardedForBlock(settings)
	if err != nil {
		return ForwardedForBlock{}, err
	}
	saved, err := editConfiguration(s, func(state *State) (ForwardedForBlock, Changes, error) {
		state.ForwardedForBlock = normalized
		return cloneForwardedForBlock(normalized), Changes{ForwardedForBlock: true}, nil
	})
	if err != nil {
		return ForwardedForBlock{}, err
	}
	s.forwardedForReports.reset()
	return saved, nil
}

const (
	forwardedForReportInterval = 10 * time.Minute
	maxForwardedForReports     = 4096
)

// forwardedForReports remembers when each key last had a refusal reported under
// each keyword. Keys come from authenticated callers and keywords from the
// operator, so the set stays small; the cap only guards against a flood of
// distinct callers.
type forwardedForReports struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reads the clock under the lock, so concurrent refusals cannot record
// their times out of order and report twice. Only a step back of a whole window
// counts as a clock change that restarts reporting.
func (r *forwardedForReports) due(key string, clock func() time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := clock()
	expired := func(at time.Time) bool {
		return now.Sub(at) >= forwardedForReportInterval || at.Sub(now) >= forwardedForReportInterval
	}
	if at, exists := r.last[key]; exists && !expired(at) {
		return false
	}
	if r.last == nil {
		r.last = make(map[string]time.Time)
	}
	if _, exists := r.last[key]; !exists && len(r.last) >= maxForwardedForReports {
		for other, at := range r.last {
			if expired(at) {
				delete(r.last, other)
			}
		}
		if len(r.last) >= maxForwardedForReports {
			return false
		}
	}
	r.last[key] = now
	return true
}

func (r *forwardedForReports) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = nil
}

// MatchForwardedForBlock reports whether the enabled rule refuses any of the
// models: a model matches when it contains a keyword, ignoring case. The caller
// decides whether the request carries X-Forwarded-For.
func (s *Store) MatchForwardedForBlock(models ...string) (ForwardedForMatch, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings := s.state.ForwardedForBlock
	if !settings.Enabled || len(settings.ModelKeywords) == 0 {
		return ForwardedForMatch{}, false
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		folded := strings.ToLower(model)
		for _, keyword := range settings.ModelKeywords {
			if keyword == "" || !strings.Contains(folded, strings.ToLower(keyword)) {
				continue
			}
			message := strings.TrimSpace(settings.Message)
			if message == "" {
				message = DefaultForwardedForBlockMessage
			}
			return ForwardedForMatch{Model: model, Keyword: keyword, Message: message}, true
		}
	}
	return ForwardedForMatch{}, false
}

// ReportForwardedForBlock records a refusal by the X-Forwarded-For rule. Like a
// quota refusal it leaves no other trace. A key is reported at most once per
// keyword within forwardedForReportInterval, so a relay that retries or mixes
// models cannot turn refused traffic into a log row per request. The caller
// passes a model already made safe to log; the header's addresses are never
// logged.
func (s *Store) ReportForwardedForBlock(scope, endpoint string, match ForwardedForMatch) {
	scope = strings.TrimSpace(scope)
	if !s.forwardedForReports.due(scope+"\x00"+match.Keyword, s.Now) {
		return
	}
	name := ""
	s.read(func(state *State) { name = state.describeKey(scope) })
	if name == "" {
		name = "未识别的 API Key"
	}

	var message strings.Builder
	message.WriteString("X-Forwarded-For 拦截：")
	message.WriteString(name)
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		message.WriteString(" → ")
		message.WriteString(endpoint)
	}
	message.WriteString("，模型 ")
	message.WriteString(match.Model)
	message.WriteString("，命中关键词 ")
	message.WriteString(match.Keyword)
	s.AddPluginLog(PluginLogInfo, "%s", message.String())
}
