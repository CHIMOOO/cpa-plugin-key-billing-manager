package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

const turnStateHostRequirement = "Turn State requires CLIProxyAPI v7.3.4-compatible HTTP/SSE forwarding. Protected Codex accounts must disable upstream WebSocket mode. Downstream WebSocket clients can then use the host's HTTP/SSE bridge, with State injected on every turn. Existing upstream WebSocket continuations may need one reconnect after this change."

type turnStateAccount struct {
	Account                string                   `json:"account"`
	Label                  string                   `json:"label"`
	Disabled               bool                     `json:"disabled"`
	AuthIndex              string                   `json:"auth_index"`
	UpstreamWebsockets     bool                     `json:"upstream_websockets"`
	UpstreamTransportKnown bool                     `json:"upstream_transport_known"`
	DisableWebsocketsPatch *turnStateWebsocketPatch `json:"disable_websockets_patch,omitempty"`
}

type turnStateStatus struct {
	turnstate.Status
	ProbeAccounts                        []turnStateAccount    `json:"probe_accounts"`
	ProbeSupported                       bool                  `json:"probe_supported"`
	ProbeUnavailableReason               string                `json:"probe_unavailable_reason,omitempty"`
	ProbeUnavailableMessage              messages.Message      `json:"probe_unavailable_message,omitzero"`
	HostRequirement                      string                `json:"host_requirement"`
	HostRequirementMessage               messages.Message      `json:"host_requirement_message,omitzero"`
	UpstreamWebsocketPatchPath           string                `json:"upstream_websocket_patch_path"`
	UpstreamWebsocketManagementSupported bool                  `json:"upstream_websocket_management_supported"`
	Runner                               turnStateRunnerStatus `json:"runner"`
	HooksRegistered                      bool                  `json:"hooks_registered"`
}

func codexAuthFile(file hostAuthFile) bool {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(file.Type))
	}
	// Explicit management probes use only this account's OAuth credential and
	// never change the host's enabled state or business-routing candidates.
	// Disabled and cooling accounts remain eligible for recovery probes.
	return provider == "codex" && file.ID != "" && file.AuthIndex != ""
}

func (a *App) getTurnState(_ ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	// Response learning publishes in memory without delaying the client on
	// disk. This explicit management call commits pending templates; any save
	// error remains visible in Status without flooding the plugin log.
	_ = a.turnState.PersistLearned()
	status := turnStateStatus{Status: a.turnState.Status(), ProbeAccounts: []turnStateAccount{},
		ProbeSupported: true, HostRequirement: turnStateHostRequirement,
		UpstreamWebsocketPatchPath:           "/v0/management/auth-files/fields",
		UpstreamWebsocketManagementSupported: a.hostSchema.Load() >= 6}
	files, err := a.listHostAuthFiles()
	if err != nil {
		status.ProbeSupported = false
		status.ProbeUnavailableReason = "This host cannot read the authentication file list"
	} else {
		for _, file := range files {
			if !codexAuthFile(file) {
				continue
			}
			label := safeCredentialName(file.Name, file.Account, "codex", billing.CredentialFingerprint(file.ID))
			row := turnStateAccount{
				Account: file.ID, Label: label,
				Disabled:  file.Disabled || strings.EqualFold(strings.TrimSpace(file.Status), "disabled"),
				AuthIndex: file.AuthIndex, UpstreamWebsockets: file.Websockets,
				UpstreamTransportKnown: a.hostSchema.Load() >= 6,
			}
			if row.UpstreamTransportKnown && !file.RuntimeOnly && (file.Path != "" || file.Source == "file") {
				row.DisableWebsocketsPatch = &turnStateWebsocketPatch{Name: file.ID}
			}
			status.ProbeAccounts = append(status.ProbeAccounts, row)
		}
	}
	status.HostRequirementMessage = messages.Literal(status.HostRequirement)
	status.Runner = a.turnStateRunner.status()
	status.HooksRegistered = a.stateHooks.Load()
	status.ProbeUnavailableMessage = messages.Literal(status.ProbeUnavailableReason)
	return JSONResponse(http.StatusOK, status)
}

func (a *App) setTurnState(req ManagementRequest) ManagementResponse {
	if err := a.turnState.Update(req.Body); err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_turn_state", messages.FromError(err))
	}
	return a.getTurnState(req)
}

func (a *App) clearTurnState(req ManagementRequest) ManagementResponse {
	var input struct {
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if len(req.Body) > 0 && json.Unmarshal(req.Body, &input) != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "Invalid template clearing parameters")
	}
	if err := a.turnState.Clear(input.Account, input.Model); err != nil {
		return jsonMessageError(http.StatusBadRequest, "invalid_turn_state", messages.FromError(err))
	}
	return a.getTurnState(req)
}

func (a *App) probeTurnState(req ManagementRequest) ManagementResponse {
	if !a.turnState.Active() {
		return turnStateSuspendedError()
	}
	finish, allowed := a.beginManualTurnStateProbe()
	if !allowed {
		return JSONError(http.StatusConflict, "runner_active", "Stop server collection before starting a manual probe")
	}
	defer finish()
	return a.executeTurnStateProbe(req)
}

func (a *App) executeTurnStateProbe(req ManagementRequest) (response ManagementResponse) {
	defer func() { response.Headers.Set("Cache-Control", "private, no-store") }()
	var input struct {
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if len(req.Body) > 0 && json.Unmarshal(req.Body, &input) != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "Invalid probe parameters")
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		return JSONError(http.StatusBadGateway, "probe_unavailable", "Failed to read the authentication file list")
	}
	available := map[string]struct{}{}
	for _, file := range files {
		if codexAuthFile(file) {
			available[file.ID] = struct{}{}
		}
	}
	result, err := a.turnState.ProbeWithAvailability(input.Account, input.Model, func(account string) bool {
		_, ok := available[account]
		return ok
	}, func(account string) (turnstate.Credential, error) {
		for _, file := range files {
			if file.ID != account || !codexAuthFile(file) {
				continue
			}
			raw, err := a.hostCaller(hostAuthGet, map[string]string{"auth_index": file.AuthIndex})
			if err != nil {
				return turnstate.Credential{}, err
			}
			var response hostAuthGetResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				return turnstate.Credential{}, err
			}
			return turnstate.ParseCredential(response.JSON, time.Now())
		}
		return turnstate.Credential{}, &turnStateMissingAccount{}
	})
	if err != nil {
		return jsonMessageError(http.StatusInternalServerError, "probe_failed", messages.FromError(err))
	}
	return JSONResponse(http.StatusOK, result)
}

func turnStateSuspendedError() ManagementResponse {
	return JSONError(http.StatusConflict, "turn_state_suspended", "Turn State is globally disabled; enable it before sending probes")
}

// turnStateGate reports whether selected accounts must hold a usable template.
// A suspended State never blocks or filters business traffic.
func (a *App) turnStateGate() bool {
	return a.turnState.Active() && a.accountRuntime != nil && a.accountRuntime.requiresTurnState()
}

type turnStateMissingAccount struct{}

func (*turnStateMissingAccount) Error() string {
	return "The probe account does not exist or is not a Codex OAuth account"
}

// Request-level provider identity must come from CPA's auth inventory. ToFormat
// alone is insufficient: xai also uses the codex format in supported hosts.
func (a *App) turnStateRequest(req RequestInterceptRequest) (http.Header, []string) {
	if a == nil || !a.turnState.Active() {
		return nil, nil
	}
	// A scheduler retry can reuse the RequestID while selecting another
	// provider. Never leave the earlier account attached to that later response.
	a.turnState.Complete(req.RequestID)
	if isTurnStateImageRequest(req.Metadata) || !a.turnState.Enabled() {
		return nil, nil
	}
	account := metadataString(req.Metadata, MetadataSelectedAuth)
	if account == "" || req.RequestID == "" {
		return nil, nil
	}
	a.routingMu.Lock()
	credential, known := a.credentials[billing.CredentialFingerprint(account)]
	a.routingMu.Unlock()
	isCodex := known && strings.EqualFold(credential.Provider, "codex")
	if !known {
		// Existing credential synchronization fills this cache. On a new account,
		// a read-only host lookup verifies attribution without guessing.
		files, err := a.listHostAuthFiles()
		if err == nil {
			for _, file := range files {
				if file.ID == account {
					isCodex = codexAuthFile(file)
					break
				}
			}
		}
	}
	if !isCodex {
		return nil, nil
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		// The client alias is not evidence of the selected upstream model.
		return nil, nil
	}
	return a.turnState.Before(req.RequestID, account, model, req.Headers)
}

func (a *App) handleTurnStateResponse(raw []byte, stream bool) ([]byte, error) {
	// Suspended State skips decoding: request.complete still drops any
	// attribution left by a request admitted before the switch.
	if !a.turnState.Active() {
		return OKEnvelope(struct{}{})
	}
	var req struct {
		RequestID       string         `json:"RequestID"`
		Model           string         `json:"Model"`
		RequestedModel  string         `json:"RequestedModel"`
		ResponseHeaders http.Header    `json:"ResponseHeaders"`
		Metadata        map[string]any `json:"Metadata"`
		ChunkIndex      int            `json:"ChunkIndex"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if isTurnStateImageRequest(req.Metadata) {
		if a.turnState != nil {
			a.turnState.Complete(req.RequestID)
		}
		return OKEnvelope(struct{}{})
	}
	if stream && req.ChunkIndex != -1 {
		return OKEnvelope(struct{}{})
	}
	if a.turnState != nil && a.store != nil && a.store.Enabled() {
		model := req.Model
		if model == "" {
			model = req.RequestedModel
		}
		if err := a.turnState.Learn(req.RequestID, metadataString(req.Metadata, MetadataSelectedAuth), model, req.ResponseHeaders); err != nil {
			// Failing to save an optional template must not break a paid response.
			a.store.AddPluginLog(billing.PluginLogError, "Failed to save the turn-state template: %v", err)
		}
	}
	return OKEnvelope(struct{}{})
}
