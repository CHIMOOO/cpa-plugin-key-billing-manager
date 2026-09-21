package plugin

import (
	"encoding/json"
	"strings"
)

type forceAstraRouteRequest struct {
	RequestedModel string         `json:"RequestedModel"`
	Metadata       map[string]any `json:"Metadata"`
}

type forceAstraRouteResponse struct {
	Handled     bool   `json:"Handled"`
	TargetKind  string `json:"TargetKind,omitempty"`
	Target      string `json:"Target,omitempty"`
	TargetModel string `json:"TargetModel,omitempty"`
}

// Route before auth selection so the executor, State and host usage all see
// Astra. Rewriting only an intercepted body is undone by executor translation
// and would leave admission and usage attributed to the original model.
func (a *App) routeForceAstra(raw []byte) ([]byte, error) {
	var req forceAstraRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	result := forceAstraRouteResponse{}
	if a == nil || a.turnState == nil || !a.turnState.ForceAstraEnabled() ||
		isTurnStateImageRequest(req.Metadata) || metadataString(req.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return OKEnvelope(result)
	}
	model := strings.ToLower(strings.TrimSpace(req.RequestedModel))
	// Match explicit GPT/Codex names only; custom aliases and other providers
	// keep their existing routes. Non-conversational GPT models are excluded.
	if (!strings.HasPrefix(model, "gpt-") && !strings.HasPrefix(model, "codex-")) ||
		strings.HasPrefix(model, "gpt-image") || strings.Contains(model, "audio") || strings.Contains(model, "realtime") ||
		strings.Contains(model, "transcribe") || strings.Contains(model, "tts") {
		return OKEnvelope(result)
	}
	target := "gpt-6-astra"
	// Keep the host's explicit reasoning suffix, e.g. gpt-5.6-sol(high).
	if start := strings.LastIndex(model, "("); start >= 0 && strings.HasSuffix(model, ")") {
		target += model[start:]
	}
	return OKEnvelope(forceAstraRouteResponse{Handled: true, TargetKind: "provider", Target: "codex", TargetModel: target})
}
