package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const integrationAuthType = "team-manager-integration"

type integrationProvider struct {
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	QuotaSupported bool   `json:"quota_supported"`
}

var integrationProviders = []integrationProvider{
	{"opencode-go", "OpenCode Go", "https://opencode.ai/zen/go/v1", true},
	{"opencode-zen", "OpenCode Zen", "https://opencode.ai/zen/v1", false},
	{"commandcode", "CommandCode", "https://api.commandcode.ai/provider/v1", true},
	{"cline-pass", "Cline Pass", "https://api.cline.bot/api/v1", true},
}

// Auxiliary credentials are owned by the host auth directory. They have a
// private provider type without an executor or models; traffic uses the host's
// OpenAI-compatible channel published by the management browser.
type integrationAccount struct {
	Type                string                   `json:"type"`
	Disabled            bool                     `json:"disabled"`
	ID                  string                   `json:"integration_id"`
	Name                string                   `json:"integration_name"`
	Kind                string                   `json:"integration_kind"`
	APIKey              string                   `json:"api_key,omitempty"`
	AuthCookie          string                   `json:"auth_cookie,omitempty"`
	RefreshToken        string                   `json:"refresh_token,omitempty"`
	WorkspaceID         string                   `json:"workspace_id,omitempty"`
	ExpiresAt           string                   `json:"expires_at,omitempty"`
	Models              []string                 `json:"integration_models,omitempty"`
	Deleted             bool                     `json:"integration_deleted,omitempty"`
	PendingRefMigration *integrationRefMigration `json:"integration_pending_ref_migration,omitempty"`
	LastRefMigration    string                   `json:"integration_last_ref_migration,omitempty"`
	LoginID             string                   `json:"integration_login_id,omitempty"`
}

type integrationView struct {
	ID                string                        `json:"id"`
	Name              string                        `json:"name"`
	Kind              string                        `json:"kind"`
	BaseURL           string                        `json:"base_url"`
	WorkspaceID       string                        `json:"workspace_id,omitempty"`
	HasAPIKey         bool                          `json:"has_api_key"`
	HasAuthCookie     bool                          `json:"has_auth_cookie"`
	HasRefreshToken   bool                          `json:"has_refresh_token"`
	ExpiresAt         string                        `json:"expires_at,omitempty"`
	Models            []string                      `json:"models"`
	ChannelName       string                        `json:"channel_name"`
	AuthFileName      string                        `json:"auth_file_name"`
	Channels          []integrationChannelView      `json:"channels"`
	CredentialRefs    []string                      `json:"credential_refs"`
	ClientModels      []string                      `json:"client_models"`
	UnsupportedModels []integrationUnsupportedModel `json:"unsupported_models"`
	PendingMigration  string                        `json:"pending_migration,omitempty"`
}

func integrationSpec(kind string) (integrationProvider, bool) {
	for _, item := range integrationProviders {
		if item.Kind == kind {
			return item, true
		}
	}
	return integrationProvider{}, false
}

func integrationFileName(id string) string { return "cpa-team-integration-" + id + ".json" }

func integrationViewOf(account integrationAccount) integrationView {
	spec, _ := integrationSpec(account.Kind)
	models := append([]string{}, account.Models...)
	return integrationView{ID: account.ID, Name: account.Name, Kind: account.Kind, BaseURL: spec.BaseURL,
		WorkspaceID: account.WorkspaceID, HasAPIKey: account.APIKey != "", HasAuthCookie: account.AuthCookie != "",
		HasRefreshToken: account.RefreshToken != "", ExpiresAt: account.ExpiresAt, Models: models,
		ChannelName: "team-" + account.Kind + "-" + account.ID, AuthFileName: integrationFileName(account.ID)}
}

func integrationJSON(status int, value any) ManagementResponse {
	response := JSONResponse(status, value)
	response.Headers.Set("Cache-Control", "no-store")
	return response
}

func integrationError(status int, detail string) ManagementResponse {
	return JSONError(status, "integration_failed", detail)
}

var integrationIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

func newIntegrationID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("Could not create an integration identifier")
	}
	return hex.EncodeToString(b[:]), nil
}

func (a *App) readIntegration(file hostAuthFile) (integrationAccount, error) {
	var account integrationAccount
	raw, err := a.hostCaller(hostAuthGet, map[string]any{"auth_index": file.AuthIndex})
	if err != nil {
		return account, fmt.Errorf("Could not read the host-managed integration credential")
	}
	var response hostAuthGetResponse
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.JSON, &account) != nil || account.Type != integrationAuthType || !integrationIDPattern.MatchString(account.ID) || file.Name != integrationFileName(account.ID) {
		return integrationAccount{}, fmt.Errorf("The host-managed integration record is invalid")
	}
	if _, ok := integrationSpec(account.Kind); !ok {
		return integrationAccount{}, fmt.Errorf("The integration provider is not supported")
	}
	return account, nil
}

func (a *App) getIntegration(id string) (integrationAccount, error) {
	if !integrationIDPattern.MatchString(id) {
		return integrationAccount{}, fmt.Errorf("Invalid integration identifier")
	}
	files, err := a.listHostAuthFiles()
	if err != nil {
		return integrationAccount{}, fmt.Errorf("Could not read the host auth directory")
	}
	for _, file := range files {
		if file.Type == integrationAuthType && file.Name == integrationFileName(id) {
			account, err := a.readIntegration(file)
			if err != nil {
				return account, err
			}
			if !account.Deleted {
				return account, nil
			}
		}
	}
	return integrationAccount{}, fmt.Errorf("Integration account not found")
}

func (a *App) persistIntegration(account integrationAccount) error {
	if a.hostCaller == nil {
		return fmt.Errorf("Host credential storage is unavailable")
	}
	account.Type, account.Disabled = integrationAuthType, true
	raw, err := json.Marshal(account)
	if err != nil {
		return fmt.Errorf("Could not encode the integration credential")
	}
	_, err = a.hostCaller("host.auth.save", map[string]any{"name": integrationFileName(account.ID), "json": json.RawMessage(raw)})
	if err != nil {
		return fmt.Errorf("Could not save the host-managed integration credential")
	}
	return nil
}

func (a *App) listIntegrations(_ ManagementRequest) ManagementResponse {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return integrationError(http.StatusBadGateway, "Could not read the host auth directory")
	}
	views := []integrationView{}
	for _, file := range files {
		if file.Type != integrationAuthType {
			continue
		}
		account, err := a.readIntegration(file)
		if err != nil {
			return integrationError(http.StatusBadGateway, err.Error())
		}
		if !account.Deleted {
			views = append(views, a.integrationView(account))
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].ID < views[j].ID
	})
	return integrationJSON(http.StatusOK, map[string]any{"accounts": views, "providers": integrationProviders})
}

type integrationInput struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	APIKey      string   `json:"api_key"`
	AuthCookie  string   `json:"auth_cookie"`
	WorkspaceID string   `json:"workspace_id"`
	Models      []string `json:"models"`
}

func (a *App) saveIntegration(req ManagementRequest) ManagementResponse {
	var input integrationInput
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	account := integrationAccount{ID: input.ID, Kind: strings.TrimSpace(input.Kind)}
	previous := integrationAccount{}
	if input.ID != "" {
		var err error
		account, _, err = a.integrationForMutation(input.ID)
		if err != nil {
			return integrationError(http.StatusNotFound, err.Error())
		}
		if account.PendingRefMigration != nil {
			return integrationError(409, "Finish the pending credential migration with Repair channels before editing this integration")
		}
		previous = account
		if input.Kind != "" && input.Kind != account.Kind {
			return integrationError(http.StatusBadRequest, "An existing integration provider cannot be changed")
		}
	} else {
		var err error
		account.ID, err = newIntegrationID()
		if err != nil {
			return integrationError(http.StatusInternalServerError, err.Error())
		}
	}
	if _, ok := integrationSpec(account.Kind); !ok {
		return integrationError(http.StatusBadRequest, "Select a supported integration provider")
	}
	account.Name = strings.TrimSpace(input.Name)
	if account.Name == "" || len(account.Name) > 160 {
		return integrationError(http.StatusBadRequest, "Enter an integration name of at most 160 characters")
	}
	for _, secret := range []string{input.APIKey, input.AuthCookie} {
		if len(secret) > 16384 || strings.ContainsAny(secret, "\r\n") {
			return integrationError(http.StatusBadRequest, "Invalid integration credential")
		}
	}
	if strings.TrimSpace(input.APIKey) != "" {
		replacement := strings.TrimSpace(input.APIKey)
		if replacement != account.APIKey {
			account.RefreshToken, account.ExpiresAt = "", ""
		}
		account.APIKey = replacement
	}
	if strings.TrimSpace(input.AuthCookie) != "" {
		account.AuthCookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input.AuthCookie), "auth="))
	}
	account.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	if account.Kind == "opencode-go" && (account.WorkspaceID == "" || len(account.WorkspaceID) > 256 || strings.ContainsAny(account.WorkspaceID, "/\\?#\r\n")) {
		return integrationError(http.StatusBadRequest, "Enter a valid OpenCode workspace ID")
	}
	if account.APIKey == "" && (account.Kind != "opencode-go" || account.AuthCookie == "") {
		return integrationError(http.StatusBadRequest, "Enter an API key or an OpenCode Go dashboard credential")
	}
	if input.Models != nil {
		models, err := normalizeIntegrationModels(input.Models)
		if err != nil {
			return integrationError(http.StatusBadRequest, err.Error())
		}
		account.Models = models
	}
	if account.Kind == "cline-pass" {
		for _, model := range account.Models {
			if !integrationClineModelAllowed(model) {
				return integrationError(http.StatusBadRequest, "Cline Pass integrations accept only supported cline-pass/ subscription models")
			}
		}
	}
	if account.APIKey != "" && len(account.Models) == 0 {
		models, err := a.fetchIntegrationModels(req.HostCallbackID, account)
		if err != nil {
			return integrationError(http.StatusBadGateway, err.Error()+"; enter explicit upstream model IDs if catalog discovery is unavailable")
		}
		account.Models = models
	}
	var err error
	account, err = a.saveIntegrationReplacement(previous, account)
	if err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	return integrationJSON(http.StatusOK, a.integrationPayload(account))
}

func normalizeIntegrationModels(input []string) ([]string, error) {
	if len(input) > 2000 {
		return nil, fmt.Errorf("At most 2000 models can be configured")
	}
	models, seen := []string{}, map[string]bool{}
	for _, raw := range input {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if len(id) > 256 || strings.ContainsAny(id, "\r\n\t") {
			return nil, fmt.Errorf("Invalid upstream model identifier")
		}
		if !seen[id] {
			models, seen[id] = append(models, id), true
		}
	}
	return models, nil
}

func integrationHeaders(kind string) http.Header {
	headers := http.Header{"Accept": {"application/json"}}
	switch kind {
	case "opencode-go", "opencode-zen":
		headers.Set("x-opencode-client", "cli")
		headers.Set("User-Agent", "opencode/1.2.27")
	case "cline-pass":
		headers.Set("x-client-type", "cli")
		headers.Set("x-client-version", "3.0.61")
		headers.Set("x-core-version", "3.0.61")
		headers.Set("User-Agent", "Cline/3.0.61")
	}
	return headers
}

func integrationAccountHeaders(account integrationAccount) http.Header {
	headers := integrationHeaders(account.Kind)
	if account.Kind == "opencode-go" || account.Kind == "opencode-zen" {
		headers.Set("x-opencode-session", "oc-team-"+account.ID)
	}
	return headers
}

func integrationRequestID(req ManagementRequest) (string, error) {
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return "", err
	}
	if !integrationIDPattern.MatchString(input.ID) {
		return "", fmt.Errorf("Invalid integration identifier")
	}
	return input.ID, nil
}

func (a *App) channelIntegration(req ManagementRequest) ManagementResponse {
	id, err := integrationRequestID(req)
	if err != nil {
		return integrationError(http.StatusBadRequest, "Invalid integration identifier")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	account, _, err := a.integrationForMutation(id)
	if err != nil {
		return integrationError(http.StatusNotFound, err.Error())
	}
	account, err = a.prepareIntegrationReplacement(account)
	if err != nil {
		return integrationError(502, err.Error())
	}
	return integrationJSON(http.StatusOK, a.integrationPayload(account))
}

func (a *App) deleteIntegration(req ManagementRequest) ManagementResponse {
	id, err := integrationRequestID(req)
	if err != nil {
		return integrationError(http.StatusBadRequest, "Invalid integration identifier")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	account, _, err := a.integrationForMutation(id)
	if err != nil {
		return integrationError(http.StatusNotFound, err.Error())
	}
	account.Deleted, account.APIKey, account.AuthCookie, account.RefreshToken, account.Models = true, "", "", "", nil
	account.PendingRefMigration = nil
	if err := a.persistIntegrationRecoverably(account); err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	return integrationJSON(http.StatusOK, map[string]any{"deleted": true})
}

// The callback keeps host proxy policy and bounds the lifetime to this call.
// Upstream bodies and errors are never relayed into logs or management errors.
func (a *App) integrationHTTP(callbackID, method, target string, headers http.Header, body []byte) (hostHTTPResponse, error) {
	var response hostHTTPResponse
	if a.hostCaller == nil {
		return response, fmt.Errorf("Host HTTP client is unavailable")
	}
	raw, err := a.hostCaller(hostHTTPDo, hostHTTPRequest{HostCallbackID: callbackID, Method: method, URL: target, Headers: headers, Body: body})
	if err != nil {
		return response, fmt.Errorf("The integration upstream request failed")
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Body) > 4<<20 {
		return hostHTTPResponse{}, fmt.Errorf("The integration upstream response is invalid or too large")
	}
	return response, nil
}

func (a *App) fetchIntegrationModels(callbackID string, account integrationAccount) ([]string, error) {
	spec, _ := integrationSpec(account.Kind)
	headers := integrationAccountHeaders(account)
	headers.Set("Authorization", "Bearer "+account.APIKey)
	response, err := a.integrationHTTP(callbackID, http.MethodGet, spec.BaseURL+"/models", headers, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Model catalog request returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data   json.RawMessage `json:"data"`
		Models json.RawMessage `json:"models"`
	}
	raw := response.Body
	if json.Unmarshal(response.Body, &payload) == nil {
		raw = payload.Data
	}
	if len(raw) == 0 {
		raw = payload.Models
	}
	var rows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	ids := []string{}
	if json.Unmarshal(raw, &rows) == nil {
		for _, row := range rows {
			ids = append(ids, firstNonEmptyString(row.ID, row.Name))
		}
	} else if json.Unmarshal(raw, &ids) != nil {
		return nil, fmt.Errorf("The model catalog format is not supported")
	}
	models, err := normalizeIntegrationModels(ids)
	if err != nil {
		return nil, err
	}
	if account.Kind == "cline-pass" {
		filtered := []string{}
		for _, model := range models {
			if integrationClineModelAllowed(model) {
				filtered = append(filtered, model)
			}
		}
		models = filtered
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("The upstream returned an empty model catalog")
	}
	return models, nil
}

func (a *App) queryIntegration(req ManagementRequest) ManagementResponse {
	id, err := integrationRequestID(req)
	if err != nil {
		return integrationError(http.StatusBadRequest, "Invalid integration identifier")
	}
	account, err := a.getIntegration(id)
	if err != nil {
		return integrationError(http.StatusNotFound, err.Error())
	}
	result := authQuotaResponse{FetchedAt: time.Now().UTC(), Quota: []quotaRow{}}
	output := map[string]any{"account": a.integrationView(account), "models": account.Models, "quota_supported": false,
		"quota_message": "An authoritative subscription quota endpoint is not available for this integration; model connectivity does not establish remaining quota."}
	if account.APIKey != "" {
		models, err := a.fetchIntegrationModels(req.HostCallbackID, account)
		if err != nil {
			output["models_error"] = err.Error()
		} else {
			output["models"] = models
		}
	}
	if account.Kind == "opencode-go" {
		if account.AuthCookie == "" {
			output["quota_message"] = "Add the OpenCode auth cookie to query workspace subscription quotas."
		} else {
			err := a.fetchOpenCodeGoQuota(req.HostCallbackID, account, &result)
			if err != nil {
				output["quota_message"] = err.Error()
			} else {
				output["quota_supported"], output["quota_message"] = true, ""
			}
		}
	}
	if account.Kind == "commandcode" || account.Kind == "cline-pass" {
		var err error
		if account.Kind == "commandcode" {
			err = a.fetchCommandCodeQuota(req.HostCallbackID, account, &result)
		} else {
			err = a.fetchClineSubscriptionQuota(req.HostCallbackID, account, &result)
		}
		if err != nil {
			output["quota_message"] = err.Error()
		} else {
			output["quota_supported"], output["quota_message"] = true, ""
		}
	}
	output["quota"] = result
	return integrationJSON(http.StatusOK, output)
}

func (a *App) fetchOpenCodeGoQuota(callbackID string, account integrationAccount, result *authQuotaResponse) error {
	headers := http.Header{"Accept": {"text/html"}, "Cookie": {"auth=" + account.AuthCookie}, "User-Agent": {"Mozilla/5.0"}}
	response, err := a.integrationHTTP(callbackID, http.MethodGet, "https://opencode.ai/workspace/"+url.PathEscape(account.WorkspaceID)+"/go", headers, nil)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenCode dashboard request returned HTTP %d", response.StatusCode)
	}
	rows := parseIntegrationOpenCodeQuota(string(response.Body), result.FetchedAt)
	if len(rows) == 0 {
		return fmt.Errorf("OpenCode dashboard quota data was not found; the cookie may have expired or the dashboard format changed")
	}
	result.Plan, result.Quota = "OpenCode Go", rows
	return nil
}

var integrationSSRWindow = regexp.MustCompile(`(rolling|weekly|monthly)Usage:\$R\[\d+\]=\{([^}]*)\}`)
var integrationSSRUsed = regexp.MustCompile(`usagePercent:(-?\d+(?:\.\d+)?)`)
var integrationSSRReset = regexp.MustCompile(`resetInSec:(-?\d+(?:\.\d+)?)`)
var integrationSlotLabel = regexp.MustCompile(`data-slot="usage-label">([^<]+)<`)
var integrationSlotValue = regexp.MustCompile(`data-slot="usage-value">[^0-9]*(\d+(?:\.\d+)?)`)

func parseIntegrationOpenCodeQuota(body string, now time.Time) []quotaRow {
	rows := map[string]quotaRow{}
	add := func(kind, usedText, resetText string) {
		used, err := strconv.ParseFloat(usedText, 64)
		if err != nil {
			return
		}
		row := quotaRow{Scope: "account", RemainingPercent: remainingPercent(100 - used)}
		switch kind {
		case "rolling":
			row.Label, row.WindowSeconds = "5-hour limit", 18000
		case "weekly":
			row.Label, row.WindowSeconds = "Weekly limit", 604800
		case "monthly":
			row.Label, row.WindowSeconds = "Monthly limit", 2592000
		default:
			return
		}
		if resetText != "" {
			if seconds, err := strconv.ParseFloat(resetText, 64); err == nil && seconds >= 0 && seconds <= 366*24*3600 {
				row.ResetAt = now.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
			}
		}
		rows[kind] = row
	}
	for _, match := range integrationSSRWindow.FindAllStringSubmatch(body, -1) {
		used, reset := integrationSSRUsed.FindStringSubmatch(match[2]), integrationSSRReset.FindStringSubmatch(match[2])
		if len(used) == 2 && len(reset) == 2 {
			add(match[1], used[1], reset[1])
		}
	}
	if len(rows) == 0 {
		for _, part := range strings.Split(html.UnescapeString(body), `data-slot="usage-item"`)[1:] {
			label, value := integrationSlotLabel.FindStringSubmatch(part), integrationSlotValue.FindStringSubmatch(part)
			if len(label) != 2 || len(value) != 2 {
				continue
			}
			name := strings.ToLower(label[1])
			kind := ""
			switch {
			case strings.Contains(name, "5"), strings.Contains(name, "rolling"):
				kind = "rolling"
			case strings.Contains(name, "week"):
				kind = "weekly"
			case strings.Contains(name, "month"):
				kind = "monthly"
			}
			add(kind, value[1], "")
		}
	}
	result := []quotaRow{}
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		if row, exists := rows[name]; exists {
			result = append(result, row)
		}
	}
	return result
}
