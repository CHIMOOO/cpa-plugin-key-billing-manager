package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const integrationClineClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

// Only the reference Cline Pass subscription catalog is published. The broad
// gateway model list also contains separately billed pay-as-you-go models.
var integrationClineModels = []string{
	"cline-pass/glm-5.3-flash", "cline-pass/glm-5.3", "cline-pass/glm-5.2",
	"cline-pass/kimi-k2.7-code", "cline-pass/kimi-k2.6", "cline-pass/kimi-k3",
	"cline-pass/deepseek-v4-pro", "cline-pass/deepseek-v4.1-flash", "cline-pass/deepseek-v4-flash",
	"cline-pass/mimo-v2.5", "cline-pass/mimo-v2.5-pro", "cline-pass/minimax-m3",
	"cline-pass/qwen3.7-plus", "cline-pass/qwen3.7-max", "cline-pass/qwen3.8-max",
}

func integrationClineModelAllowed(id string) bool {
	for _, candidate := range integrationClineModels {
		if id == candidate {
			return true
		}
	}
	return false
}

type integrationLogin struct {
	Name       string
	DeviceCode string
	ExpiresAt  time.Time
	NextPoll   time.Time
	Interval   int
	Account    *integrationAccount
}

func (a *App) startIntegrationCline(req ManagementRequest) ManagementResponse {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 160 {
		return integrationError(http.StatusBadRequest, "Enter an integration name of at most 160 characters")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	if a.integrationLogins == nil {
		a.integrationLogins = map[string]*integrationLogin{}
	}
	now := time.Now().UTC()
	for id, login := range a.integrationLogins {
		if !now.Before(login.ExpiresAt) {
			delete(a.integrationLogins, id)
		}
	}
	if len(a.integrationLogins) >= 16 {
		return integrationError(http.StatusTooManyRequests, "Too many pending Cline sign-ins")
	}
	form := url.Values{"client_id": {integrationClineClientID}}
	response, err := a.integrationHTTP(req.HostCallbackID, http.MethodPost, "https://api.workos.com/user_management/authorize/device", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
	if err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &payload) != nil || payload.DeviceCode == "" || payload.UserCode == "" {
		return integrationError(http.StatusBadGateway, "Cline device authorization was rejected")
	}
	if !validIntegrationLoginURL(payload.VerificationURI) || (payload.VerificationURIComplete != "" && !validIntegrationLoginURL(payload.VerificationURIComplete)) {
		return integrationError(http.StatusBadGateway, "Cline returned an invalid sign-in URL")
	}
	if payload.Interval < 5 {
		payload.Interval = 5
	}
	if payload.Interval > 60 {
		payload.Interval = 60
	}
	if payload.ExpiresIn < 30 || payload.ExpiresIn > 900 {
		payload.ExpiresIn = 900
	}
	id, err := newIntegrationID()
	if err != nil {
		return integrationError(http.StatusInternalServerError, err.Error())
	}
	expires := now.Add(time.Duration(payload.ExpiresIn) * time.Second)
	a.integrationLogins[id] = &integrationLogin{Name: input.Name, DeviceCode: payload.DeviceCode, ExpiresAt: expires, NextPoll: now.Add(time.Duration(payload.Interval) * time.Second), Interval: payload.Interval}
	return integrationJSON(http.StatusOK, map[string]any{"login_id": id, "user_code": payload.UserCode, "verification_uri": payload.VerificationURI, "verification_uri_complete": payload.VerificationURIComplete, "expires_at": expires, "interval": payload.Interval})
}

func validIntegrationLoginURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "cline.bot" || strings.HasSuffix(host, ".cline.bot") || host == "workos.com" || strings.HasSuffix(host, ".workos.com")
}

func (a *App) pollIntegrationCline(req ManagementRequest) ManagementResponse {
	var input struct {
		LoginID string `json:"login_id"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	login := a.integrationLogins[input.LoginID]
	now := time.Now().UTC()
	if login == nil || !now.Before(login.ExpiresAt) {
		delete(a.integrationLogins, input.LoginID)
		return integrationError(http.StatusGone, "Cline sign-in expired; start again")
	}
	if login.Account == nil {
		if now.Before(login.NextPoll) {
			return integrationJSON(http.StatusOK, map[string]any{"status": "pending", "interval": login.Interval})
		}
		login.NextPoll = now.Add(time.Duration(login.Interval) * time.Second)
		form := url.Values{"client_id": {integrationClineClientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {login.DeviceCode}}
		response, err := a.integrationHTTP(req.HostCallbackID, http.MethodPost, "https://api.workos.com/user_management/authenticate", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(form.Encode()))
		if err != nil {
			return integrationError(http.StatusBadGateway, err.Error())
		}
		var payload struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
		}
		if json.Unmarshal(response.Body, &payload) != nil {
			return integrationError(http.StatusBadGateway, "Cline authorization returned an invalid response")
		}
		if payload.Error == "authorization_pending" || payload.Error == "slow_down" {
			if payload.Error == "slow_down" {
				login.Interval += 5
				login.NextPoll = now.Add(time.Duration(login.Interval) * time.Second)
			}
			return integrationJSON(http.StatusOK, map[string]any{"status": "pending", "interval": login.Interval})
		}
		if response.StatusCode != http.StatusOK || payload.AccessToken == "" || payload.RefreshToken == "" {
			delete(a.integrationLogins, input.LoginID)
			return integrationError(http.StatusBadGateway, "Cline sign-in was rejected; start again")
		}
		// Keep received credentials in this short-lived in-memory session until
		// host persistence succeeds, so retrying a save never reuses the grant.
		id, err := newIntegrationID()
		if err != nil {
			return integrationError(http.StatusInternalServerError, err.Error())
		}
		account := &integrationAccount{ID: id, Name: login.Name, Kind: "cline-pass", APIKey: payload.AccessToken, RefreshToken: payload.RefreshToken, LoginID: input.LoginID}
		login.Account = account
	}
	account := login.Account
	if !strings.HasPrefix(account.APIKey, "workos:") {
		body, _ := json.Marshal(map[string]string{"accessToken": account.APIKey, "refreshToken": account.RefreshToken})
		response, err := a.integrationHTTP(req.HostCallbackID, http.MethodPost, "https://api.cline.bot/api/v1/auth/register", http.Header{"Content-Type": {"application/json"}}, body)
		if err != nil {
			return integrationError(http.StatusBadGateway, err.Error())
		}
		access, refresh, expires, err := integrationClineTokens(response)
		if err != nil {
			return integrationError(http.StatusBadGateway, err.Error())
		}
		account.APIKey, account.ExpiresAt = integrationClinePrefix(access), expires
		if refresh != "" {
			account.RefreshToken = refresh
		}
	}
	if err := a.persistIntegration(*account); err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	models, err := a.fetchIntegrationModels(req.HostCallbackID, *account)
	output := map[string]any{"status": "complete"}
	if err != nil {
		output["models_error"] = err.Error()
	} else {
		account.Models = models
		if err := a.persistIntegration(*account); err != nil {
			return integrationError(http.StatusBadGateway, err.Error())
		}
	}
	for name, value := range a.integrationPayload(*account) {
		output[name] = value
	}
	delete(a.integrationLogins, input.LoginID)
	return integrationJSON(http.StatusOK, output)
}

func integrationClinePrefix(token string) string {
	return "workos:" + strings.TrimPrefix(strings.TrimSpace(token), "workos:")
}

// The same mutex covers the whole poll, including its host save. A cancel
// therefore either removes a pending grant or observes the completed account;
// it never reports cancellation while a concurrent poll can still publish it.
func (a *App) cancelIntegrationCline(req ManagementRequest) ManagementResponse {
	var input struct {
		LoginID string `json:"login_id"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	if !integrationIDPattern.MatchString(input.LoginID) {
		return integrationError(http.StatusBadRequest, "Invalid Cline sign-in identifier")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	login := a.integrationLogins[input.LoginID]
	if login != nil && login.Account == nil {
		delete(a.integrationLogins, input.LoginID)
		return integrationJSON(http.StatusOK, map[string]any{"status": "cancelled"})
	}
	// Completed polls remove their session. The host-owned correlation ID also
	// lets a reconnect or restart distinguish completion from cancellation.
	files, err := a.listHostAuthFiles()
	if err != nil {
		return integrationError(http.StatusBadGateway, "Could not confirm whether Cline sign-in completed; retry cancellation")
	}
	for _, file := range files {
		if file.Type != integrationAuthType {
			continue
		}
		account, err := a.readIntegration(file)
		if err != nil {
			return integrationError(http.StatusBadGateway, "Could not confirm whether Cline sign-in completed; retry cancellation")
		}
		if account.LoginID == input.LoginID && !account.Deleted {
			delete(a.integrationLogins, input.LoginID)
			return integrationJSON(http.StatusOK, map[string]any{"status": "complete", "account": a.integrationView(account)})
		}
	}
	delete(a.integrationLogins, input.LoginID)
	if login != nil && login.Account != nil {
		delete(a.integrationUnsaved, login.Account.ID)
	}
	return integrationJSON(http.StatusOK, map[string]any{"status": "cancelled"})
}

func integrationClineTokens(response hostHTTPResponse) (string, string, string, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", "", fmt.Errorf("Cline token exchange returned HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if json.Unmarshal(response.Body, &payload) != nil {
		return "", "", "", fmt.Errorf("Cline token exchange returned an invalid response")
	}
	if success, ok := payload["success"].(bool); ok && !success {
		return "", "", "", fmt.Errorf("Cline token exchange was rejected")
	}
	if data := objectMap(payload, "data"); data != nil {
		payload = data
	}
	access, refresh := firstString(payload, "accessToken", "access_token"), firstString(payload, "refreshToken", "refresh_token")
	if access == "" || strings.ContainsAny(access+refresh, "\r\n") {
		return "", "", "", fmt.Errorf("Cline token exchange returned no usable access token")
	}
	expires := quotaResetAt(firstString(payload, "expiresAt", "expires_at"))
	return access, refresh, expires, nil
}

func (a *App) refreshIntegration(req ManagementRequest) ManagementResponse {
	id, err := integrationRequestID(req)
	if err != nil {
		return integrationError(http.StatusBadRequest, "Invalid integration identifier")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	account, recovering, err := a.integrationForMutation(id)
	if err != nil {
		return integrationError(http.StatusNotFound, err.Error())
	}
	if account.PendingRefMigration != nil || recovering {
		account, err = a.prepareIntegrationReplacement(account)
		if err != nil {
			return integrationError(502, err.Error())
		}
		return integrationJSON(200, a.integrationPayload(account))
	}
	if account.Kind != "cline-pass" || account.RefreshToken == "" {
		return integrationError(http.StatusBadRequest, "This account has no Cline refresh token; sign in again")
	}
	body, _ := json.Marshal(map[string]string{"granttype": "refresh_token", "refreshToken": account.RefreshToken})
	response, err := a.integrationHTTP(req.HostCallbackID, http.MethodPost, "https://api.cline.bot/api/v1/auth/refresh", http.Header{"Content-Type": {"application/json"}}, body)
	if err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	access, refresh, expires, err := integrationClineTokens(response)
	if err != nil {
		return integrationError(http.StatusBadGateway, err.Error())
	}
	previous := account
	account.APIKey, account.ExpiresAt = integrationClinePrefix(access), expires
	if refresh != "" {
		account.RefreshToken = refresh
	}
	account, err = a.saveIntegrationReplacement(previous, account)
	if err != nil {
		return integrationError(http.StatusBadGateway, "The token rotated but its channel migration is pending; use Repair channels to retry without rotating again. "+err.Error())
	}
	return integrationJSON(http.StatusOK, a.integrationPayload(account))
}
