package billing

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// QuotaScope selects client-visible billing models, not upstream credentials.
// Empty scopes retain the historical all-model quota behavior. Every matching
// window applies, so an overall budget can coexist with narrower limits.
type QuotaScope struct {
	Models    []string `json:"models,omitempty"`
	Providers []string `json:"providers,omitempty"`
}

const maxQuotaScopeModels = 1024

// Persisted scope mistakes must not silently broaden a window to all models.
// The outer management decoder is strict too, but SQLite JSON uses Unmarshal.
func (q *QuotaScope) UnmarshalJSON(raw []byte) error {
	type plainScope QuotaScope
	var decoded plainScope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	normalized, err := QuotaScope(decoded).normalized()
	if err == nil {
		*q = normalized
	}
	return err
}

func (q QuotaScope) IsZero() bool { return len(q.Models) == 0 && len(q.Providers) == 0 }

func (q QuotaScope) clone() QuotaScope {
	return QuotaScope{Models: slices.Clone(q.Models), Providers: slices.Clone(q.Providers)}
}

func (q QuotaScope) normalized() (QuotaScope, error) {
	if len(q.Models) > maxQuotaScopeModels || len(q.Providers) > 6 {
		return QuotaScope{}, invalidf("A quota scope supports at most %d model IDs or 6 model families", maxQuotaScopeModels)
	}
	if len(q.Models) > 0 && len(q.Providers) > 0 {
		return QuotaScope{}, invalidf("A quota scope must select models or model families, not both")
	}
	out := QuotaScope{}
	for _, raw := range q.Models {
		model := NormalizeModelID(raw)
		if model == "" || len(model) > maxRouteValueBytes || strings.ContainsAny(model, "*[]\x00\r\n") {
			return QuotaScope{}, invalidf("Quota scope models must be non-empty exact model identifiers")
		}
		out.Models = append(out.Models, model)
	}
	for _, raw := range q.Providers {
		provider := strings.ToLower(strings.TrimSpace(raw))
		switch provider {
		case "openai", "claude", "xai", "gemini", "deepseek", "qwen":
		default:
			return QuotaScope{}, invalidf("Unsupported quota model family %q", raw)
		}
		out.Providers = append(out.Providers, provider)
	}
	slices.Sort(out.Models)
	out.Models = slices.Compact(out.Models)
	slices.Sort(out.Providers)
	out.Providers = slices.Compact(out.Providers)
	return out, nil
}

func (q QuotaScope) identity() string {
	normalized, _ := q.normalized()
	raw, _ := json.Marshal(normalized)
	return string(raw)
}

func (q QuotaScope) matches(model string) bool {
	if q.IsZero() {
		return true
	}
	model = NormalizeModelID(model)
	for _, candidate := range q.Models {
		if NormalizeModelID(candidate) == model {
			return true
		}
	}
	family := quotaModelFamily(model)
	return family != "" && slices.Contains(q.Providers, family)
}

// Classification is deliberately identical before admission and in usage.handle:
// use the billing identity, never the selected executor or upstream alias. CPA
// route prefixes and thinking options do not change a model's family. Unknown
// custom names cannot safely be assigned to a family and need an exact-model
// scope. An unmatched model is still subject to any all-model window.
func quotaModelFamily(model string) string {
	model = NormalizeModelID(ModelWithoutThinkingSuffix(model))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	switch {
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "chatgpt-"),
		strings.HasPrefix(model, "codex-"), reasoningModelName(model):
		return "openai"
	case strings.HasPrefix(model, "claude-"):
		return "claude"
	case strings.HasPrefix(model, "grok-"):
		return "xai"
	case strings.HasPrefix(model, "gemini-"), strings.HasPrefix(model, "gemma-"):
		return "gemini"
	case strings.HasPrefix(model, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(model, "qwen-") || strings.HasPrefix(model, "qwen2") || strings.HasPrefix(model, "qwen3"):
		return "qwen"
	default:
		return ""
	}
}

func reasoningModelName(model string) bool {
	if len(model) < 2 || model[0] != 'o' || model[1] < '0' || model[1] > '9' {
		return false
	}
	for _, ch := range model[2:] {
		if ch == '-' {
			return true
		}
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (p Plan) forModel(model string) Plan {
	selected := Plan{ID: p.ID, Name: p.Name}
	for _, window := range p.Windows {
		if window.Scope.matches(model) {
			selected.Windows = append(selected.Windows, window)
		}
	}
	return selected
}
