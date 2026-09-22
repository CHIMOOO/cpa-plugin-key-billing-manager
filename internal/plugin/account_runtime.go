package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"cpa-key-billing/internal/billing"
)

type accountRuntimePolicy struct {
	ConcurrencyLimit int `json:"concurrency_limit"`
}
type accountRuntimeSettings struct {
	Accounts          map[string]accountRuntimePolicy `json:"accounts"`
	RequireTurnState  bool                            `json:"require_turn_state"`
	CredentialAliases map[string]string               `json:"credential_aliases,omitempty"`
}
type accountRuntime struct {
	mu       sync.Mutex
	settings accountRuntimeSettings
	path     string
	active   map[string]int
	requests map[string]string
}

func newAccountRuntime() *accountRuntime {
	return &accountRuntime{settings: defaultAccountRuntimeSettings(), active: map[string]int{}, requests: map[string]string{}}
}
func defaultAccountRuntimeSettings() accountRuntimeSettings {
	return accountRuntimeSettings{Accounts: map[string]accountRuntimePolicy{}, RequireTurnState: true}
}

var accountRefPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func validateAccountRuntimeSettings(s accountRuntimeSettings) error {
	if len(s.Accounts) > 20000 || len(s.CredentialAliases) > 20000 {
		return errors.New("Too many account policies")
	}
	for ref, p := range s.Accounts {
		if !accountRefPattern.MatchString(ref) || p.ConcurrencyLimit < 0 || p.ConcurrencyLimit > 1000 {
			return errors.New("Account concurrency requires a credential reference and a limit between 0 and 1000")
		}
	}
	for from, to := range s.CredentialAliases {
		if !accountRefPattern.MatchString(from) || !accountRefPattern.MatchString(to) {
			return errors.New("Invalid account credential alias")
		}
	}
	_, err := flattenAccountAliases(s.CredentialAliases)
	return err
}

func flattenAccountAliases(aliases map[string]string) (map[string]string, error) {
	resolved := map[string]string{}
	for start := range aliases {
		if resolved[start] != "" {
			continue
		}
		path := []string{}
		seen := map[string]bool{}
		ref := start
		for aliases[ref] != "" && resolved[ref] == "" {
			if seen[ref] {
				return nil, errors.New("Account credential aliases contain a cycle")
			}
			seen[ref] = true
			path = append(path, ref)
			ref = aliases[ref]
		}
		if root := resolved[ref]; root != "" {
			ref = root
		}
		for _, item := range path {
			resolved[item] = ref
		}
	}
	return resolved, nil
}

func resolveAccountAlias(aliases map[string]string, ref string) (string, error) {
	seen := map[string]bool{}
	for aliases[ref] != "" {
		if seen[ref] {
			return "", errors.New("Account credential aliases contain a cycle")
		}
		seen[ref] = true
		ref = aliases[ref]
	}
	return ref, nil
}
func accountCanonicalRef(s accountRuntimeSettings, ref string) string {
	canonical, _ := resolveAccountAlias(s.CredentialAliases, ref)
	return canonical
}
func stricterAccountLimit(a, b int) int {
	if a == 0 {
		return b
	}
	if b == 0 || a < b {
		return a
	}
	return b
}
func cloneAccountRuntimeSettings(s accountRuntimeSettings) accountRuntimeSettings {
	s.Accounts = maps.Clone(s.Accounts)
	if s.Accounts == nil {
		s.Accounts = map[string]accountRuntimePolicy{}
	}
	s.CredentialAliases = maps.Clone(s.CredentialAliases)
	if s.CredentialAliases == nil {
		s.CredentialAliases = map[string]string{}
	}
	return s
}
func normalizeAccountAliases(s *accountRuntimeSettings) error {
	if s.Accounts == nil {
		s.Accounts = map[string]accountRuntimePolicy{}
	}
	if err := validateAccountRuntimeSettings(*s); err != nil {
		return err
	}
	s.CredentialAliases, _ = flattenAccountAliases(s.CredentialAliases)
	limits := map[string]int{}
	for ref, policy := range s.Accounts {
		root := accountCanonicalRef(*s, ref)
		limits[root] = stricterAccountLimit(limits[root], policy.ConcurrencyLimit)
	}
	for ref, root := range s.CredentialAliases {
		s.Accounts[ref] = accountRuntimePolicy{limits[root]}
		s.Accounts[root] = accountRuntimePolicy{limits[root]}
	}
	for ref := range s.Accounts {
		s.Accounts[ref] = accountRuntimePolicy{limits[accountCanonicalRef(*s, ref)]}
	}
	return validateAccountRuntimeSettings(*s)
}

// migrateCredentialRefs keeps old and newly published credentials in one pool,
// including requests already executing during a token rotation. Only opaque
// refs are persisted; old aliases remain valid after the UI commits a channel.
func (r *accountRuntime) migrateCredentialRefs(mapping map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	mapping = maps.Clone(mapping)
	for from, to := range mapping {
		if from == to {
			delete(mapping, from)
		}
	}
	if len(mapping) == 0 {
		return nil
	}
	if err := validateAccountRuntimeSettings(accountRuntimeSettings{CredentialAliases: mapping}); err != nil {
		return err
	}
	next := cloneAccountRuntimeSettings(r.settings)
	keys := make([]string, 0, len(mapping))
	for ref := range mapping {
		keys = append(keys, ref)
	}
	sort.Strings(keys)
	for _, from := range keys {
		to := mapping[from]
		if from == to {
			continue
		}
		oldRoot, newRoot := accountCanonicalRef(next, from), accountCanonicalRef(next, to)
		if oldRoot != newRoot {
			next.CredentialAliases[oldRoot] = newRoot
		}
		if from != newRoot {
			next.CredentialAliases[from] = newRoot
		}
	}
	if err := normalizeAccountAliases(&next); err != nil {
		return err
	}
	if err := writePrivateJSON(r.path, next); err != nil {
		return err
	}
	active := map[string]int{}
	for id, ref := range r.requests {
		root := accountCanonicalRef(next, ref)
		r.requests[id] = root
		active[root]++
	}
	r.settings, r.active = next, active
	return nil
}
func loadAccountRuntimeSettings(path string) (accountRuntimeSettings, error) {
	s := defaultAccountRuntimeSettings()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, errors.New("Cannot read account runtime settings")
	}
	if len(raw) > 4<<20 || decodeStrict(raw, &s) != nil {
		return s, errors.New("Invalid account runtime settings")
	}
	err = normalizeAccountAliases(&s)
	return s, err
}
func (r *accountRuntime) snapshot() accountRuntimeSettings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneAccountRuntimeSettings(r.settings)
}
func (r *accountRuntime) requiresTurnState() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings.RequireTurnState
}
func (r *accountRuntime) hasConcurrencyLimits() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, policy := range r.settings.Accounts {
		if policy.ConcurrencyLimit > 0 {
			return true
		}
	}
	return false
}
func (r *accountRuntime) acquire(ref, requestID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref = accountCanonicalRef(r.settings, ref)
	if previous := r.requests[requestID]; previous == ref && previous != "" {
		return true
	} else if previous != "" {
		r.active[previous]--
		delete(r.requests, requestID)
	}
	limit := r.settings.Accounts[ref].ConcurrencyLimit
	if requestID == "" {
		return limit == 0
	}
	if limit > 0 && r.active[ref] >= limit {
		return false
	}
	if len(r.requests) >= 100000 {
		return false
	}
	r.requests[requestID] = ref
	r.active[ref]++
	return true
}
func (r *accountRuntime) release(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref := r.requests[requestID]; ref != "" {
		r.active[ref]--
		if r.active[ref] <= 0 {
			delete(r.active, ref)
		}
		delete(r.requests, requestID)
	}
}

// writePrivateJSON never exposes a partial configuration after a failed save.
func writePrivateJSON(path string, value any) error {
	if path == "" {
		return errors.New("Settings storage is not configured")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("Cannot create settings directory")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cpa-settings-*")
	if err != nil {
		return errors.New("Cannot create settings file")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return errors.New("Cannot save settings file")
	}
	return nil
}

func (a *App) setAccountRuntimeSettings(req ManagementRequest) ManagementResponse {
	r := a.accountRuntime
	r.mu.Lock()
	defer r.mu.Unlock()
	next := cloneAccountRuntimeSettings(r.settings)
	if len(req.Body) > 4<<20 || decodeStrict(req.Body, &next) != nil {
		return JSONError(400, "invalid_account_settings", "Invalid account runtime settings")
	}
	if err := validateAccountRuntimeSettings(next); err != nil {
		return JSONError(400, "invalid_account_settings", err.Error())
	}
	if !maps.Equal(next.CredentialAliases, r.settings.CredentialAliases) {
		return JSONError(400, "invalid_account_settings", "Credential rotation aliases are managed internally")
	}
	// A user edit applies to the complete merged pool. An omitted aliases field
	// never clears rotation protection, including when accounts is replaced.
	var patch struct {
		Accounts map[string]accountRuntimePolicy `json:"accounts"`
	}
	_ = json.Unmarshal(req.Body, &patch)
	updates := map[string]int{}
	for ref, policy := range patch.Accounts {
		root := accountCanonicalRef(next, ref)
		if previous, exists := updates[root]; exists {
			updates[root] = stricterAccountLimit(previous, policy.ConcurrencyLimit)
		} else {
			updates[root] = policy.ConcurrencyLimit
		}
	}
	for ref := range next.Accounts {
		if limit, exists := updates[accountCanonicalRef(next, ref)]; exists {
			next.Accounts[ref] = accountRuntimePolicy{limit}
		}
	}
	for ref, root := range next.CredentialAliases {
		if limit, exists := updates[root]; exists {
			next.Accounts[ref] = accountRuntimePolicy{limit}
			next.Accounts[root] = accountRuntimePolicy{limit}
		}
	}
	if err := normalizeAccountAliases(&next); err != nil {
		return JSONError(400, "invalid_account_settings", err.Error())
	}
	if err := writePrivateJSON(r.path, next); err != nil {
		return JSONError(500, "account_settings_save_failed", err.Error())
	}
	r.settings = next
	return JSONResponse(200, next)
}
func (a *App) getAccountRuntimeSettings(_ ManagementRequest) ManagementResponse {
	return JSONResponse(200, a.accountRuntime.snapshot())
}
func (a *App) getAccountRuntime(_ ManagementRequest) ManagementResponse {
	a.pruneModelTests()
	files, err := a.listHostAuthFiles()
	if err != nil {
		return JSONError(502, "host_unavailable", "Failed to read the authentication file list")
	}
	usage, err := a.store.AccountUsage()
	if err != nil {
		return JSONError(500, "usage_unavailable", "Failed to read account usage")
	}
	byIndex := map[string]billing.AccountUsage{}
	for _, u := range usage {
		byIndex[u.AuthIndex] = u
	}
	type row struct {
		CredentialRef      string                `json:"credential_ref"`
		AuthIndex          string                `json:"auth_index"`
		Name               string                `json:"name"`
		Provider           string                `json:"provider"`
		Disabled           bool                  `json:"disabled"`
		ConcurrencyLimit   int                   `json:"concurrency_limit"`
		CurrentConcurrency int                   `json:"current_concurrency"`
		UsageAvailable     bool                  `json:"usage_available"`
		Usage              *billing.AccountUsage `json:"usage"`
		Source             string                `json:"source,omitempty"`
		StatusPatch        *accountStatusPatch   `json:"status_patch,omitempty"`
		StatusToggleReason string                `json:"status_toggle_reason,omitempty"`
	}
	rows := []row{}
	settings := a.accountRuntime.snapshot()
	active := map[string]int{}
	a.accountRuntime.mu.Lock()
	for ref, count := range a.accountRuntime.active {
		active[ref] = count
	}
	a.accountRuntime.mu.Unlock()
	seen := map[string]bool{}
	for _, f := range files {
		if f.ID == "" || f.AuthIndex == "" || strings.EqualFold(f.Type, integrationAuthType) || strings.EqualFold(f.Provider, integrationAuthType) {
			continue
		}
		ref := billing.CredentialFingerprint(f.ID)
		provider := f.Provider
		if provider == "" {
			provider = f.Type
		}
		u := byIndex[f.AuthIndex]
		u.AuthIndex = f.AuthIndex
		rows = append(rows, row{CredentialRef: ref, AuthIndex: f.AuthIndex,
			Name: credentialDisplayName(f, credentialSourceFromHost(f), provider, ref), Provider: provider,
			Disabled: f.Disabled || strings.EqualFold(f.Status, "disabled"), ConcurrencyLimit: settings.Accounts[ref].ConcurrencyLimit,
			CurrentConcurrency: active[accountCanonicalRef(settings, ref)], UsageAvailable: true, Usage: &u,
			Source: credentialSourceFromHost(f)})
		seen[ref] = true
	}
	// Some hosts omit config credentials from host.auth.list. Only the exact
	// index/ref association supplied by after-auth metadata may fill that gap.
	a.routingMu.Lock()
	for index, ref := range a.credentialRefsByIndex {
		credential, ok := a.credentials[ref]
		if !ok || seen[ref] || credential.Source != billing.CredentialSourceAIProviders || credential.Provider == integrationAuthType {
			continue
		}
		u := byIndex[index]
		u.AuthIndex = index
		rows = append(rows, row{CredentialRef: ref, AuthIndex: index, Name: credential.DisplayName, Provider: credential.Provider,
			Disabled: credential.Disabled, ConcurrencyLimit: settings.Accounts[ref].ConcurrencyLimit,
			CurrentConcurrency: active[accountCanonicalRef(settings, ref)], UsageAvailable: true, Usage: &u, Source: credential.Source})
		seen[ref] = true
	}
	for ref, credential := range a.credentials {
		if seen[ref] || credential.Source != billing.CredentialSourceAIProviders || credential.Provider == integrationAuthType {
			continue
		}
		// The opaque ref is sufficient to configure admission before first use.
		// Usage remains explicitly unavailable until the host supplies its index.
		rows = append(rows, row{CredentialRef: ref, Name: credential.DisplayName, Provider: credential.Provider,
			Disabled: credential.Disabled, ConcurrencyLimit: settings.Accounts[ref].ConcurrencyLimit,
			CurrentConcurrency: active[accountCanonicalRef(settings, ref)], Source: credential.Source})
	}
	a.routingMu.Unlock()
	// Resolve management descriptors after releasing routingMu. A missing
	// native inventory entry needs an exact local runtime callback, never a
	// guessed account name or a reversed credential fingerprint.
	identities := accountStatusIdentities(files)
	for i := range rows {
		item := &rows[i]
		file, known := identities.byRef[item.CredentialRef]
		if !known && item.AuthIndex != "" {
			var runtimeErr error
			file, runtimeErr = a.modelTestAccount(item.AuthIndex)
			known = runtimeErr == nil && billing.CredentialFingerprint(file.ID) == item.CredentialRef &&
				strings.EqualFold(accountStatusProvider(file), item.Provider)
			if known {
				item.Disabled = file.Disabled || strings.EqualFold(file.Status, "disabled")
				item.Source = credentialSourceFromHost(file)
			}
		}
		if !known || file.AuthIndex != item.AuthIndex || identities.ambiguous[item.CredentialRef] {
			item.StatusToggleReason = "unverified_identity"
			continue
		}
		item.StatusPatch, item.StatusToggleReason = a.accountStatusDescriptor(file, identities.pathCounts)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].AuthIndex == rows[j].AuthIndex {
			return rows[i].CredentialRef < rows[j].CredentialRef
		}
		return rows[i].AuthIndex < rows[j].AuthIndex
	})
	return JSONResponse(200, map[string]any{"accounts": rows, "settings": settings, "usage_retention_days": 365, "token_totals": "classified_reported_tokens", "host_schema": a.hostSchema.Load(), "turn_state_host_supported": a.hostSchema.Load() >= 6, "status_patch_path": "/v0/management/auth-files/status"})
}

func (a *App) enforceAccountRuntime(req RequestInterceptRequest) RequestInterceptResponse {
	if a != nil {
		a.pruneModelTests()
	}
	if a == nil || a.accountRuntime == nil || a.store == nil || !a.store.Enabled() || metadataString(req.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return RequestInterceptResponse{}
	}
	id := metadataString(req.Metadata, MetadataSelectedAuth)
	protectState := a.turnStateGate() && !isTurnStateImageRequest(req.Metadata)
	if id == "" {
		if protectState && a.turnState.HasProtectedAccounts() {
			return priceRefusal(req.SourceFormat, "turn_state_required", "Cannot verify the selected account; protected account requests are blocked")
		}
		if a.accountRuntime.hasConcurrencyLimits() {
			return priceRefusal(req.SourceFormat, "account_identity_required", "Cannot verify the selected account; configured account concurrency limits cannot be enforced")
		}
		return RequestInterceptResponse{}
	}
	ref := billing.CredentialFingerprint(id)
	// Only the selected accounts' selected models go through State.
	protectState = protectState && a.turnState.Protects(id, req.Model)
	if protectState && a.hostSchema.Load() < 6 {
		return priceRefusal(req.SourceFormat, "turn_state_host_unsupported", "Protected Turn State accounts require a verified CLIProxyAPI v7.3.4-compatible host with plugin schema 6 or newer")
	}
	if protectState && (strings.EqualFold(strings.TrimSpace(req.Headers.Get("Upgrade")), "websocket") || metadataString(req.Metadata, "execution_session_id") != "") {
		if !a.turnStateUpstreamHTTP(req, id) {
			return priceRefusal(req.SourceFormat, "turn_state_websocket_unsupported", "This protected account must disable upstream WebSocket mode before serving WebSocket clients. Save its State account selection to apply HTTP/SSE mode, then reconnect existing sessions")
		}
	}
	if !a.accountRuntime.acquire(ref, req.RequestID) {
		return RequestInterceptResponse{Terminate: true, StatusCode: http.StatusTooManyRequests, ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"1"}}, ResponseBody: refusalBody(req.SourceFormat, quotaExhaustedError, fmt.Sprintf("Upstream account concurrency limit reached for %s", ref[:15]))}
	}
	return RequestInterceptResponse{}
}
