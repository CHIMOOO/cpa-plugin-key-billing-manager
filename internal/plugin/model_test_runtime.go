package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
	"cpa-key-billing/internal/turnstate"
)

const (
	modelTestMaxActive        = 4
	modelTestPromptBytes      = 8192
	modelTestLeaseDuration    = 90 * time.Second
	modelTestSendWindow       = 10 * time.Second
	modelTestMaxResponseBytes = 1 << 20
)

// Only a random capability, opaque credential ref, assertion, and deadlines
// remain in memory. Prompts, output, token headers, and proxies are never stored.
type modelTestLease struct {
	RequestID      string
	Protocol       string
	Preset         string
	Expected       string
	RequestedModel string
	UpstreamModel  string
	Until          time.Time
}

type modelTestAccount struct {
	IdentitySource string           `json:"identity_source,omitempty"`
	CredentialRef  string           `json:"credential_ref"`
	AuthIndex      string           `json:"auth_index"`
	Name           string           `json:"name"`
	Provider       string           `json:"provider"`
	Source         string           `json:"source"`
	Disabled       bool             `json:"disabled"`
	BaseURL        string           `json:"base_url,omitempty"`
	Supported      bool             `json:"supported"`
	Reason         string           `json:"reason,omitempty"`
	ReasonMessage  messages.Message `json:"reason_message,omitzero"`
}

type modelTestConfig struct {
	AuthID        string `json:"auth_id,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	AuthIndex     string `json:"auth_index,omitempty"`
	Disabled      *bool  `json:"disabled,omitempty"`
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	Protocol      string `json:"protocol"`
	Models        []struct {
		Name  string `json:"name"`
		Alias string `json:"alias,omitempty"`
	} `json:"models"`
	ProxyURL *string           `json:"proxy_url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type modelTestPrepareInput struct {
	AuthIndex      string           `json:"auth_index"`
	Model          string           `json:"model"`
	Preset         string           `json:"preset"`
	Prompt         string           `json:"prompt,omitempty"`
	Expected       string           `json:"expected,omitempty"`
	GlobalProxyURL *string          `json:"global_proxy_url,omitempty"`
	Config         *modelTestConfig `json:"config,omitempty"`
}

type modelTestAPICall struct {
	proxySource string
	AuthIndex   string            `json:"auth_index"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	ProxyURL    string            `json:"proxy_url"`
	Header      map[string]string `json:"header"`
	Data        string            `json:"data"`
}

type modelTestPrepared struct {
	TestID         string           `json:"test_id"`
	ExpiresAt      time.Time        `json:"expires_at"`
	StartBefore    time.Time        `json:"start_before"`
	LeaseExpiresAt time.Time        `json:"lease_expires_at"`
	Account        modelTestAccount `json:"account"`
	Model          string           `json:"model"`
	Preset         string           `json:"preset"`
	Proxy          struct {
		Source   string `json:"source"`
		Endpoint string `json:"endpoint"`
	} `json:"proxy"`
	APICall        modelTestAPICall `json:"api_call"`
	UsageAvailable bool             `json:"usage_available"`
}

func (a *App) modelTestTime() time.Time {
	if a.modelTestNow != nil {
		return a.modelTestNow().UTC()
	}
	return time.Now().UTC()
}

func (a *App) pruneModelTestsLocked(now time.Time) {
	for id, lease := range a.modelTests {
		if !now.Before(lease.Until) {
			a.accountRuntime.release(lease.RequestID)
			delete(a.modelTests, id)
		}
	}
}

// Also called from normal business admission, so closing a test page cannot
// retain a concurrency slot indefinitely. No timer or background worker runs.
func (a *App) pruneModelTests() {
	a.modelTestsMu.Lock()
	defer a.modelTestsMu.Unlock()
	a.pruneModelTestsLocked(a.modelTestTime())
}

func modelTestAccountView(file hostAuthFile) modelTestAccount {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(file.Type))
	}
	source := credentialSourceFromHost(file)
	ref := billing.CredentialFingerprint(file.ID)
	view := modelTestAccount{CredentialRef: ref, AuthIndex: file.AuthIndex, Provider: provider, Source: source,
		Name: credentialDisplayName(file, source, provider, ref), BaseURL: file.BaseURL,
		Disabled: file.Disabled || strings.EqualFold(file.Status, "disabled")}
	view.Supported = file.ID != "" && file.AuthIndex != "" && (source == billing.CredentialSourceAIProviders || source == billing.CredentialSourceAuthFiles && provider == "codex")
	if !view.Supported {
		view.Reason = "This account has no supported native model test protocol"
	}
	// A management API-call targets this exact credential and bypasses normal
	// account selection. Disabled accounts remain available for diagnostics;
	// preparing a test never changes their business-routing enabled state.
	view.ReasonMessage = messages.Literal(view.Reason)
	return view
}

func (a *App) getModelTests(_ ManagementRequest) ManagementResponse {
	a.pruneModelTests()
	files, err := a.listHostAuthFiles()
	if err != nil {
		return modelTestError(502, "Cannot read the host account inventory")
	}
	accounts := []modelTestAccount{}
	for _, file := range files {
		if strings.EqualFold(file.Provider, integrationAuthType) || strings.EqualFold(file.Type, integrationAuthType) {
			continue
		}
		accounts = append(accounts, modelTestAccountView(file))
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Name < accounts[j].Name })
	return modelTestJSON(200, map[string]any{"accounts": accounts, "presets": modelTestPresets, "usage_available": false,
		"limits": map[string]int{"max_active": modelTestMaxActive, "max_prompt_bytes": modelTestPromptBytes, "lease_seconds": 90, "send_within_seconds": 10, "max_response_bytes": modelTestMaxResponseBytes}})
}

func modelTestJSON(status int, value any) ManagementResponse {
	if result, ok := value.(modelTestResult); ok {
		result.ReasonMessage = messages.Literal(result.Reason)
		value = result
	}
	response := JSONResponse(status, value)
	response.Headers.Set("Cache-Control", "private, no-store")
	response.Headers.Set("Pragma", "no-cache")
	return response
}

func modelTestError(status int, message string) ManagementResponse {
	response := JSONError(status, "model_test_unavailable", message)
	response.Headers.Set("Cache-Control", "private, no-store")
	return response
}

func validModelTestText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n") && !strings.Contains(value, "$TOKEN$")
}

// This is an authenticated management workflow. The descriptor is a freshly
// read native configuration snapshot, without API keys. The runtime callback
// proves the exact credential identity; no hash is reversed into an AuthID.
func (a *App) modelTestAccount(index string) (hostAuthFile, error) {
	if a.hostCaller == nil {
		return hostAuthFile{}, errors.New("The host cannot resolve account identity")
	}
	raw, err := a.hostCaller("host.auth.get_runtime", map[string]string{"auth_index": index})
	if err != nil {
		return hostAuthFile{}, errors.New("The host cannot verify this exact runtime account")
	}
	var result struct {
		Auth hostAuthFile `json:"auth"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Auth.ID == "" || result.Auth.AuthIndex != index {
		return hostAuthFile{}, errors.New("The host returned an invalid runtime account identity")
	}
	if result.Auth.Provider == integrationAuthType || result.Auth.Type == integrationAuthType {
		return hostAuthFile{}, errors.New("Integration storage records cannot execute model tests")
	}
	return result.Auth, nil
}

// Older host inventories omit configuration credentials. A management caller
// may supply a current native-GET snapshot with its opaque StableID and the
// host-issued auth-index. This is explicitly an administrator-attested snapshot,
// not a claim that host.auth.get_runtime independently verified the index.
func (a *App) modelTestSnapshotAccount(input modelTestPrepareInput) (hostAuthFile, error) {
	cfg := input.Config
	if cfg == nil || cfg.AuthIndex != input.AuthIndex || cfg.Disabled == nil || cfg.AuthID == "" || cfg.CredentialRef != billing.CredentialFingerprint(cfg.AuthID) {
		return hostAuthFile{}, errors.New("Refresh the current native account configuration and its exact auth index")
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	kind := ""
	switch provider {
	case "codex", "claude", "gemini":
		kind = provider + ":apikey"
	default:
		if provider == "openai-compatibility" {
			kind = "openai-compatibility:" + provider
		} else if strings.HasPrefix(provider, "openai-compatible-") {
			kind = "openai-compatibility:" + strings.TrimPrefix(provider, "openai-compatible-")
		}
	}
	if kind == "" || len(provider) > 160 || !regexp.MustCompile("^"+regexp.QuoteMeta(kind)+`:[a-f0-9]{12}(?:-[1-9][0-9]*)?$`).MatchString(cfg.AuthID) {
		return hostAuthFile{}, errors.New("The native account identifier does not match its provider")
	}
	if saved, ok := a.store.ConfigCredentials()[cfg.CredentialRef]; ok && (saved.Disabled != *cfg.Disabled || !strings.EqualFold(saved.Provider, provider)) {
		return hostAuthFile{}, errors.New("This native account conflicts with the current synchronized credential inventory")
	}
	a.routingMu.Lock()
	knownRef := a.credentialRefsByIndex[input.AuthIndex]
	knownIDRef := a.credentialsByRawID[cfg.AuthID]
	a.routingMu.Unlock()
	if knownRef != "" && knownRef != cfg.CredentialRef || knownIDRef != "" && knownIDRef != cfg.CredentialRef {
		return hostAuthFile{}, errors.New("This native account index conflicts with an observed host identity")
	}
	return hostAuthFile{ID: cfg.AuthID, AuthIndex: input.AuthIndex, Provider: provider, Source: "config", RuntimeOnly: true, BaseURL: cfg.BaseURL, Disabled: *cfg.Disabled}, nil
}

func (a *App) prepareModelTest(req ManagementRequest) ManagementResponse {
	if len(req.Body) > 64<<10 {
		return modelTestError(413, "The model test request is too large")
	}
	var input modelTestPrepareInput
	if err := decodeStrict(req.Body, &input); err != nil {
		return modelTestError(400, "Invalid model test request")
	}
	if !modelTestProxyInputTypes(req.Body) {
		return modelTestError(400, "A configured proxy must be a string; null cannot mean direct")
	}
	if !validModelTestText(input.AuthIndex, 256) || !validModelTestText(input.Model, 256) {
		return modelTestError(400, "Choose an exact account and model")
	}
	if a.hostSchema.Load() < 6 {
		return modelTestError(409, "Model tests require a CLIProxyAPI v7.3.4-compatible host with explicit API-call proxy support")
	}
	if input.Preset == "" {
		input.Preset = "free"
	}
	prompt, expected, err := modelTestPrompt(input.Preset, input.Prompt, input.Expected)
	if err != nil {
		return modelTestError(400, err.Error())
	}
	file, err := a.modelTestAccount(input.AuthIndex)
	identitySource := "host-runtime"
	if err != nil && input.Config != nil {
		file, err = a.modelTestSnapshotAccount(input)
		identitySource = "management-snapshot"
	}
	if err != nil {
		return modelTestError(409, err.Error())
	}
	account := modelTestAccountView(file)
	account.IdentitySource = identitySource
	if cfg := input.Config; cfg != nil {
		if cfg.Disabled != nil && *cfg.Disabled != account.Disabled || cfg.AuthID != "" && cfg.AuthID != file.ID || cfg.AuthIndex != "" && cfg.AuthIndex != file.AuthIndex || cfg.CredentialRef != "" && cfg.CredentialRef != account.CredentialRef {
			return modelTestError(409, "The submitted native configuration does not match the selected account")
		}
	}
	if !account.Supported {
		return modelTestError(409, account.Reason)
	}
	call, protocol, actualModel, err := a.modelTestNativeRequest(file, input, prompt)
	if err != nil {
		return modelTestError(400, err.Error())
	}
	var token [24]byte
	if _, err = rand.Read(token[:]); err != nil {
		return modelTestError(500, "Cannot allocate a model test")
	}
	id := hex.EncodeToString(token[:])
	requestID := "model-test:" + id
	now := a.modelTestTime()
	prepared := modelTestPrepared{TestID: id, StartBefore: now.Add(modelTestSendWindow), ExpiresAt: now.Add(modelTestLeaseDuration), LeaseExpiresAt: now.Add(modelTestLeaseDuration), Account: account, Model: actualModel, Preset: input.Preset, APICall: call}
	prepared.Proxy.Source, prepared.Proxy.Endpoint = modelTestProxyView(call)
	// Apply the same model/content policy without recording raw test prompts.
	if blocked := a.risk.inspect(RequestInterceptRequest{RequestID: requestID, Model: actualModel, Body: []byte(call.Data)}); blocked.Terminate {
		return modelTestError(blocked.StatusCode, "The configured content risk policy blocked this model test")
	}
	if account.Provider == "codex" && a.turnState.Active() {
		headers := http.Header{}
		for k, v := range prepared.APICall.Header {
			headers.Set(k, v)
		}
		var updates http.Header
		if a.turnStateGate() {
			var reason string
			updates, _, reason = a.turnState.BeforeRequired(requestID, file.ID, actualModel, headers)
			if reason != "" {
				a.turnState.Complete(requestID)
				return modelTestError(409, reason)
			}
		} else {
			updates, _ = a.turnState.Before(requestID, file.ID, actualModel, headers)
		}
		a.turnState.Complete(requestID)
		if value := updates.Get(turnstate.Header); value != "" {
			prepared.APICall.Header[turnstate.Header] = value
			for _, bucket := range a.turnState.Status().Templates {
				if bucket.Account == file.ID && bucket.Model == turnstate.ModelName(actualModel) && bucket.ExpiresAt.Add(-time.Second).Before(prepared.StartBefore) {
					prepared.StartBefore = bucket.ExpiresAt.Add(-time.Second)
				}
			}
			if !now.Before(prepared.StartBefore) {
				return modelTestError(409, "The Turn State template expires too soon to start this test")
			}
		}
	}
	a.modelTestsMu.Lock()
	defer a.modelTestsMu.Unlock()
	a.pruneModelTestsLocked(now)
	if len(a.modelTests) >= modelTestMaxActive {
		return modelTestError(429, "At most four model tests may be active")
	}
	if !a.accountRuntime.acquire(account.CredentialRef, requestID) {
		return modelTestError(429, "This account has reached its concurrency limit")
	}
	if a.modelTests == nil {
		a.modelTests = map[string]modelTestLease{}
	}
	a.modelTests[id] = modelTestLease{RequestID: requestID, Protocol: protocol, Preset: input.Preset, Expected: expected, RequestedModel: input.Model, UpstreamModel: actualModel, Until: prepared.LeaseExpiresAt}
	return modelTestJSON(200, prepared)
}

func modelTestProxyInputTypes(raw []byte) bool {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	valid := func(value json.RawMessage) bool {
		if len(value) == 0 {
			return true
		}
		var text string
		return strings.TrimSpace(string(value)) != "null" && json.Unmarshal(value, &text) == nil
	}
	if !valid(input["global_proxy_url"]) {
		return false
	}
	var cfg map[string]json.RawMessage
	if len(input["config"]) > 0 && json.Unmarshal(input["config"], &cfg) != nil {
		return false
	}
	return valid(cfg["proxy_url"])
}

func modelTestProxy(value *string) (string, error) {
	if value == nil || *value == "" {
		return "", nil
	}
	if strings.TrimSpace(*value) == "" {
		return "", errors.New("A configured proxy cannot contain only whitespace")
	}
	raw := strings.TrimSpace(*value)
	if strings.EqualFold(raw, "direct") || strings.EqualFold(raw, "none") {
		return "direct", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || strings.ContainsAny(raw, "\x00\r\n") || len(raw) > 4096 {
		return "", errors.New("The configured proxy is invalid; the test will not fall back to a direct connection")
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return "", errors.New("The configured proxy scheme is not supported")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("The configured proxy port is invalid")
		}
	}
	if u.User != nil && (len(u.User.Username()) > 255) {
		return "", errors.New("The configured proxy username is too long")
	}
	return raw, nil
}

func modelTestEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\x00\r\n") || len(raw) > 2048 {
		return "", errors.New("The configured upstream base URL is invalid")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func modelTestHeaders(values map[string]string) (map[string]string, error) {
	headers := map[string]string{"Content-Type": "application/json"}
	if len(values) > 16 {
		return nil, errors.New("Too many custom model test headers")
	}
	for name, value := range values {
		switch strings.ToLower(name) {
		case "openai-organization", "openai-project", "anthropic-version", "anthropic-beta", "x-title", "http-referer", "accept", "user-agent", "x-opencode-session", "x-client-type", "x-client-version", "x-core-version":
		default:
			return nil, errors.New("This account uses unsupported custom headers; secret or routing headers cannot be submitted to model tests")
		}
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "$TOKEN$") {
			return nil, errors.New("Invalid custom model test header")
		}
		headers[http.CanonicalHeaderKey(name)] = value
	}
	return headers, nil
}

func (a *App) modelTestNativeRequest(file hostAuthFile, input modelTestPrepareInput, prompt string) (modelTestAPICall, string, string, error) {
	var call modelTestAPICall
	account := modelTestAccountView(file)
	protocol, base, actualModel := "", "", strings.TrimSpace(input.Model)
	var accountProxy *string
	headers := map[string]string{}
	codexOAuth := account.Source == billing.CredentialSourceAuthFiles && account.Provider == "codex"
	if codexOAuth {
		if input.Config != nil {
			return call, "", "", errors.New("File account settings are read from the host, not a submitted descriptor")
		}
		raw, err := a.hostCaller(hostAuthGet, map[string]string{"auth_index": file.AuthIndex})
		var response hostAuthGetResponse
		var material map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.JSON, &material) != nil {
			return call, "", "", errors.New("Cannot read this account's native test configuration")
		}
		credential, credentialErr := turnstate.ParseCredential(response.JSON, a.modelTestTime())
		if credentialErr != nil {
			return call, "", "", errors.New("This file does not contain a Codex OAuth access token")
		}
		if field, exists := material["proxy_url"]; exists {
			var value string
			if string(field) == "null" || json.Unmarshal(field, &value) != nil {
				return call, "", "", errors.New("The account proxy configuration is invalid")
			}
			accountProxy = &value
		}
		accountID := credential.AccountID
		if strings.ContainsAny(accountID, "\x00\r\n") || strings.Contains(accountID, "$TOKEN$") {
			return call, "", "", errors.New("Invalid Codex account identifier")
		}
		if accountID != "" {
			headers["Chatgpt-Account-Id"] = accountID
		}
		headers["Originator"] = "codex-tui"
		headers["User-Agent"] = "codex-tui/0.154.0 (Linux; x86_64)"
		protocol, base = "openai-responses", "https://chatgpt.com/backend-api/codex"
		if file.BaseURL != "" {
			base = file.BaseURL
		}
	} else {
		cfg := input.Config
		if account.Source != billing.CredentialSourceAIProviders || cfg == nil {
			return call, "", "", errors.New("Read this account's current native provider configuration before testing")
		}
		if !strings.EqualFold(strings.TrimSpace(cfg.Provider), account.Provider) {
			return call, "", "", errors.New("The provider descriptor does not match this exact runtime account")
		}
		if file.BaseURL != "" && strings.TrimRight(strings.TrimSpace(file.BaseURL), "/") != strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/") {
			return call, "", "", errors.New("The provider base URL changed; reload its configuration")
		}
		var err error
		headers, err = modelTestHeaders(cfg.Headers)
		if err != nil {
			return call, "", "", err
		}
		protocol, base, accountProxy = cfg.Protocol, cfg.BaseURL, cfg.ProxyURL
		if account.Provider == "codex" && protocol != "openai-responses" || account.Provider == "claude" && protocol != "claude" || account.Provider == "gemini" && protocol != "gemini" {
			return call, "", "", errors.New("The native protocol does not match this account provider")
		}
		if len(cfg.Models) > 1000 {
			return call, "", "", errors.New("Too many configured model mappings")
		}
		if len(cfg.Models) > 0 {
			found := ""
			for _, model := range cfg.Models {
				if model.Name == actualModel || model.Alias != "" && model.Alias == actualModel {
					if found != "" && found != model.Name {
						return call, "", "", errors.New("The selected model alias is ambiguous")
					}
					found = model.Name
				}
			}
			if found == "" {
				return call, "", "", errors.New("The selected model is outside this account's configured model list")
			}
			actualModel = found
		}
	}
	if !validModelTestText(actualModel, 256) {
		return call, "", "", errors.New("Invalid upstream model identifier")
	}
	proxy, err := modelTestProxy(accountProxy)
	if err != nil {
		return call, "", "", err
	}
	proxySource := "account"
	if proxy == "" {
		proxySource = "global"
		proxy, err = modelTestProxy(input.GlobalProxyURL)
		if err != nil {
			return call, "", "", err
		}
	}
	if proxy == "" {
		proxySource = "direct"
		proxy = "direct"
	}
	if base == "" {
		switch protocol {
		case "claude":
			base = "https://api.anthropic.com"
		case "gemini":
			base = "https://generativelanguage.googleapis.com"
		case "openai-responses":
			base = "https://api.openai.com/v1"
		}
	}
	base, err = modelTestEndpoint(base)
	if err != nil {
		return call, "", "", err
	}
	headers["Content-Type"] = "application/json"
	var payload map[string]any
	switch protocol {
	case "openai":
		base += "/chat/completions"
		headers["Authorization"] = "Bearer $TOKEN$"
		// Compatible providers disagree on the optional token-limit field;
		// omit it rather than guessing from a model name. The host bounds the
		// request lifetime and completion bounds the displayed response size.
		payload = map[string]any{"model": actualModel, "messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": false}
	case "openai-responses":
		base += "/responses"
		headers["Authorization"] = "Bearer $TOKEN$"
		payload = map[string]any{"model": actualModel, "input": []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}}, "instructions": "", "store": false, "stream": codexOAuth}
		if codexOAuth {
			headers["Accept"] = "text/event-stream"
		} else {
			payload["max_output_tokens"] = 2048
		}
	case "claude":
		base += "/v1/messages"
		headers["X-Api-Key"] = "$TOKEN$"
		if headers["Anthropic-Version"] == "" {
			headers["Anthropic-Version"] = "2023-06-01"
		}
		payload = map[string]any{"model": actualModel, "messages": []map[string]string{{"role": "user", "content": prompt}}, "max_tokens": 2048, "stream": false}
	case "gemini":
		if strings.ContainsAny(actualModel, "/?#\\") {
			return call, "", "", errors.New("Invalid Gemini model identifier")
		}
		base += "/v1beta/models/" + url.PathEscape(actualModel) + ":generateContent"
		headers["X-Goog-Api-Key"] = "$TOKEN$"
		payload = map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}}, "generationConfig": map[string]any{"maxOutputTokens": 2048}}
	default:
		return call, "", "", errors.New("This native provider protocol is not supported by model tests")
	}
	body, _ := json.Marshal(payload)
	call = modelTestAPICall{proxySource: proxySource, AuthIndex: file.AuthIndex, Method: http.MethodPost, URL: base, ProxyURL: proxy, Header: headers, Data: string(body)}
	return call, protocol, actualModel, nil
}

func modelTestProxyView(call modelTestAPICall) (string, string) {
	source, proxy := call.proxySource, call.ProxyURL
	if proxy == "direct" {
		return source, "direct"
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return source, "invalid"
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	return source, u.Scheme + "://" + func() string {
		if u.User != nil {
			return "***@"
		}
		return ""
	}() + u.Host
}
