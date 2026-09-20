package plugin

import (
	"encoding/json"
	"net/http"
	"strings"

	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

func (a *App) clearTurnStateCooldowns(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Confirm bool   `json:"confirm"`
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	if !input.Confirm {
		return JSONError(http.StatusBadRequest, "confirmation_required", "Confirm cooldown clearing before retrying upstream requests")
	}
	cleared, err := a.turnState.ClearCooldowns(input.Account, input.Model)
	if err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_turn_state", messages.FromError(err))
	}
	// A successful local mutation does not depend on a host inventory refresh.
	return JSONResponse(http.StatusOK, struct {
		turnstate.Status
		Cleared int `json:"cleared"`
	}{a.turnState.Status(), cleared})
}

type turnStateSelfTestRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	EntryProtocol  string      `json:"entry_protocol"`
	ExitProtocol   string      `json:"exit_protocol"`
	Model          string      `json:"model"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	ForcedProvider string      `json:"forced_provider"`
	AuthID         string      `json:"auth_id"`
}

type turnStateSelfTestResponse struct {
	Account   string `json:"account"`
	Model     string `json:"model"`
	Status    int    `json:"status"`
	Length    int    `json:"length"`
	Reached   bool   `json:"reached"`
	Harvested bool   `json:"harvested"`
	Reason    string `json:"reason"`
}

// The host callback pins the exact credential and exercises CPA's normal path.
// CPA deliberately skips this plugin's interceptors on nested execution, so a
// self-test never harvests state. Neither raw response nor opaque host errors
// are forwarded: they may contain credentials, and usage.handle owns failure
// logging. The reported status/length are direct diagnostic observations only.
func (a *App) selfTestTurnState(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Account string `json:"account"`
		Model   string `json:"model"`
		Confirm bool   `json:"confirm"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	input.Account, input.Model = strings.TrimSpace(input.Account), turnstate.ModelName(input.Model)
	if input.Account == "" || input.Model == "" || len(input.Account) > 4096 || len(input.Model) > 4096 || strings.ContainsAny(input.Account+input.Model, "\x00\r\n") {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "A specific account and model are required for a self-test")
	}
	if !input.Confirm {
		return JSONError(http.StatusBadRequest, "confirmation_required", "Confirm the self-test; it sends one upstream request and may consume quota")
	}
	finish, allowed := a.beginManualTurnStateProbe()
	if !allowed {
		return JSONError(http.StatusConflict, "runner_active", "Stop server collection before starting a manual probe")
	}
	defer finish()
	if a.hostCaller == nil {
		return JSONError(http.StatusBadGateway, "probe_unavailable", "Failed to read the authentication file list")
	}
	files, err := a.listHostAuthFiles()
	status := a.turnState.Status()
	known := false
	// Configured Codex API-key credentials can produce verified templates but
	// are omitted from host.auth.list on some hosts. An existing exact bucket
	// still supports a pinned diagnostic; the host validates current existence.
	for _, row := range status.Templates {
		if row.Account == input.Account && row.Model == input.Model {
			known = true
			break
		}
	}
	if err != nil && !known {
		return JSONError(http.StatusBadGateway, "probe_unavailable", "Failed to read the authentication file list")
	}
	for _, file := range files {
		if file.ID == input.Account && codexAuthFile(file) {
			known = true
			break
		}
	}
	if !known {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "The probe account does not exist or is not a Codex OAuth account")
	}
	// Permit selected models and historical buckets outside the current scope.
	known = false
	for _, model := range status.Config.Models {
		if model == input.Model {
			known = true
			break
		}
	}
	for _, row := range status.Templates {
		if row.Account == input.Account && row.Model == input.Model {
			known = true
			break
		}
	}
	if !known {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "The model is outside the saved probe scope")
	}
	body, _ := json.Marshal(map[string]any{"model": input.Model, "input": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "ping"}}}}, "store": false})
	raw, err := a.hostCaller("host.model.execute", turnStateSelfTestRequest{HostCallbackID: req.HostCallbackID, EntryProtocol: "openai-responses", ExitProtocol: "openai-responses", Model: input.Model, Body: body, Headers: http.Header{"Content-Type": {"application/json"}}, ForcedProvider: "codex", AuthID: input.Account})
	result := turnStateSelfTestResponse{Account: input.Account, Model: input.Model, Reason: "Self-test completed without saving a template; use harvesting to fill the bucket"}
	if err != nil {
		result.Reason = "The host could not complete this self-test; check CPA's request errors and account status"
		return JSONResponse(http.StatusOK, result)
	}
	var upstream struct {
		Status  int         `json:"status_code"`
		Headers http.Header `json:"headers"`
	}
	if json.Unmarshal(raw, &upstream) != nil {
		return JSONError(http.StatusBadGateway, "probe_unavailable", "The host returned an invalid self-test response")
	}
	result.Status = upstream.Status
	result.Reached = upstream.Status >= 100 && upstream.Status <= 599
	for header, values := range upstream.Headers {
		if strings.EqualFold(header, turnstate.Header) && len(values) > 0 {
			result.Length = len(strings.TrimSpace(values[0]))
			break
		}
	}
	return JSONResponse(http.StatusOK, result)
}
