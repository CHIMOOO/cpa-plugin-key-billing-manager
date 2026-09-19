package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/turnstate"
)

const turnStateHostRequirement = "HTTP/SSE 注入需要 CLIProxyAPI v7.3.4 或已包含 X-Codex-Turn-State 转发支持的版本；v7.2.143 会在 Codex HTTP executor 丢弃此请求头。WebSocket 仅新建连接握手可注入，复用连接无法逐条更新。"

type turnStateAccount struct {
	Account string `json:"account"`
	Label   string `json:"label"`
}

type turnStateStatus struct {
	turnstate.Status
	ProbeAccounts          []turnStateAccount `json:"probe_accounts"`
	ProbeSupported         bool               `json:"probe_supported"`
	ProbeUnavailableReason string             `json:"probe_unavailable_reason,omitempty"`
	HostRequirement        string             `json:"host_requirement"`
}

func codexAuthFile(file hostAuthFile) bool {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(file.Type))
	}
	// A management caller must not use the probe endpoint to bypass CPA's
	// account switch. Unavailable may describe a different model's cooldown;
	// it must not prevent explicitly requested recovery probes for every model.
	return provider == "codex" && file.ID != "" && file.AuthIndex != "" && !file.Disabled &&
		!strings.EqualFold(strings.TrimSpace(file.Status), "disabled")
}

func (a *App) getTurnState(_ ManagementRequest) ManagementResponse {
	status := turnStateStatus{Status: a.turnState.Status(), ProbeAccounts: []turnStateAccount{},
		ProbeSupported: turnstate.ProbeSupported(), HostRequirement: turnStateHostRequirement}
	files, err := a.listHostAuthFiles()
	if err != nil {
		status.ProbeSupported = false
		status.ProbeUnavailableReason = "当前宿主不能读取认证文件列表"
	} else {
		for _, file := range files {
			if !codexAuthFile(file) {
				continue
			}
			label := safeCredentialName(file.Name, file.Account, "codex", billing.CredentialFingerprint(file.ID))
			status.ProbeAccounts = append(status.ProbeAccounts, turnStateAccount{Account: file.ID, Label: label})
		}
		if !status.ProbeSupported {
			status.ProbeUnavailableReason = "服务器需要安装 curl 才能执行同步探测"
		}
	}
	return JSONResponse(http.StatusOK, status)
}

func (a *App) setTurnState(req ManagementRequest) ManagementResponse {
	if err := a.turnState.Update(req.Body); err != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", err.Error())
	}
	return a.getTurnState(req)
}

func (a *App) clearTurnState(req ManagementRequest) ManagementResponse {
	var input struct {
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if len(req.Body) > 0 && json.Unmarshal(req.Body, &input) != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "清理模板参数无效")
	}
	if err := a.turnState.Clear(input.Account, input.Model); err != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", err.Error())
	}
	return a.getTurnState(req)
}

func (a *App) probeTurnState(req ManagementRequest) ManagementResponse {
	var input struct {
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if len(req.Body) > 0 && json.Unmarshal(req.Body, &input) != nil {
		return JSONError(http.StatusBadRequest, "invalid_turn_state", "探测参数无效")
	}
	if !turnstate.ProbeSupported() {
		return JSONError(http.StatusServiceUnavailable, "probe_unavailable", "服务器未安装 curl")
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		return JSONError(http.StatusBadGateway, "probe_unavailable", "无法读取认证文件列表")
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
		return JSONError(http.StatusInternalServerError, "probe_failed", err.Error())
	}
	return JSONResponse(http.StatusOK, result)
}

type turnStateMissingAccount struct{}

func (*turnStateMissingAccount) Error() string {
	return "探测账号不存在或不是 Codex OAuth 账号"
}

// Request-level provider identity must come from CPA's auth inventory. ToFormat
// alone is insufficient: xai also uses the codex format in supported hosts.
func (a *App) turnStateRequest(req RequestInterceptRequest) (http.Header, []string) {
	if a == nil || a.turnState == nil {
		return nil, nil
	}
	// A scheduler retry can reuse the RequestID while selecting another
	// provider. Never leave the earlier account attached to that later response.
	a.turnState.Complete(req.RequestID)
	if !a.turnState.Enabled() {
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
			a.store.AddPluginLog(billing.PluginLogError, "保存 turn-state 模板失败：%v", err)
		}
	}
	return OKEnvelope(struct{}{})
}
