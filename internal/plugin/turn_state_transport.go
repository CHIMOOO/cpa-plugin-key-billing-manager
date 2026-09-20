package plugin

import (
	"encoding/json"
	"strings"
)

// The browser submits this exact, narrow patch to the existing same-origin
// management endpoint. host.auth.save would replace the whole credential and
// its runtime flags; it is deliberately not used for a transport-only edit.
type turnStateWebsocketPatch struct {
	Name       string `json:"name"`
	Websockets bool   `json:"websockets"`
}

// A downstream WebSocket does not imply an upstream WebSocket. Supported CPA
// CodexAutoExecutor uses HTTP/SSE for each turn when this account's WebSocket
// mode is false. Read the current host runtime for every protected WS request:
// a management-page cache would miss an external change back to WebSocket.
//
// This callback stays inside the host process and does not read OAuth JSON or
// send network traffic. It is not the selected execution snapshot, so a
// concurrent external mode change cannot be made atomic with auth selection.
// Existing connection-bound continuations may require a replay. No atomic
// transport override exists in the current request-interception API.
func (a *App) turnStateUpstreamHTTP(req RequestInterceptRequest, account string) bool {
	if a == nil || a.hostCaller == nil || a.hostSchema.Load() < 6 {
		return false
	}
	index := metadataString(req.Metadata, MetadataSelectedIndex)
	if account == "" || index == "" {
		return false
	}
	raw, err := a.hostCaller("host.auth.get_runtime", map[string]string{"auth_index": index})
	if err != nil {
		return false
	}
	var result struct {
		Auth struct {
			ID         string          `json:"id"`
			AuthIndex  string          `json:"auth_index"`
			Provider   string          `json:"provider"`
			Type       string          `json:"type"`
			Websockets json.RawMessage `json:"websockets"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	file := result.Auth
	// Supported host contracts omit websockets:false, and CodexAutoExecutor
	// likewise defaults an absent value to HTTP. Exact runtime identity and
	// provider verification are still mandatory; an absent auth object fails.
	provider := strings.TrimSpace(file.Provider)
	if provider == "" {
		provider = strings.TrimSpace(file.Type)
	}
	return file.ID == account && file.AuthIndex == index && strings.EqualFold(provider, "codex") &&
		(len(file.Websockets) == 0 || strings.TrimSpace(string(file.Websockets)) == "false")
}
