package plugin

import (
	"cpa-key-billing/internal/turnstate"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestAccountRuntimeConcurrencyRetryAndCompletion(t *testing.T) {
	a := newConfiguredApp(t)
	refA, refB := billing.CredentialFingerprint("dummy-a"), billing.CredentialFingerprint("dummy-b")
	response := a.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, accountRuntimeSettings{RequireTurnState: true, Accounts: map[string]accountRuntimePolicy{refA: {1}, refB: {1}}})})
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	if got := turnStateAfterAuth(t, a, "r1", "dummy-a", "model", nil); got.Terminate {
		t.Fatal(got)
	}
	if got := turnStateAfterAuth(t, a, "r1", "dummy-a", "model", nil); got.Terminate {
		t.Fatal("duplicate admission refused")
	}
	if got := turnStateAfterAuth(t, a, "r2", "dummy-a", "model", nil); !got.Terminate || got.StatusCode != 429 {
		t.Fatal("account concurrency not enforced", got)
	}
	if got := turnStateAfterAuth(t, a, "r1", "dummy-b", "model", nil); got.Terminate {
		t.Fatal("fallback to B refused", got)
	}
	if got := turnStateAfterAuth(t, a, "r2", "dummy-a", "model", nil); got.Terminate {
		t.Fatal("fallback leaked slot A", got)
	}
	for _, id := range []string{"r1", "r2"} {
		if _, err := a.completeRequest(mustMarshal(t, RequestCompletion{RequestID: id})); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.accountRuntime.requests) != 0 || len(a.accountRuntime.active) != 0 {
		t.Fatal("completion leaked account slots")
	}
	loaded, err := loadAccountRuntimeSettings(a.accountRuntime.path)
	if err != nil || loaded.Accounts[refA].ConcurrencyLimit != 1 || !loaded.RequireTurnState {
		t.Fatal("settings not persisted", err)
	}
	before, _ := os.ReadFile(a.accountRuntime.path)
	if got := a.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, map[string]any{"accounts": map[string]accountRuntimePolicy{refA: {-1}}})}); got.StatusCode != 400 {
		t.Fatal("invalid limit accepted")
	}
	after, _ := os.ReadFile(a.accountRuntime.path)
	if string(before) != string(after) {
		t.Fatal("failed save mutated settings")
	}
}

func TestAccountConcurrencyParallelLimitIsAtomic(t *testing.T) {
	r := newAccountRuntime()
	r.settings.Accounts["ref"] = accountRuntimePolicy{ConcurrencyLimit: 3}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); r.acquire("ref", string(rune(i+65))) }(i)
	}
	wg.Wait()
	if r.active["ref"] != 3 || len(r.requests) != 3 {
		t.Fatal("parallel admissions exceeded limit")
	}
}

func TestProtectedStateBlocksMissingModelsConfigurationAndFallback(t *testing.T) {
	a := newConfiguredApp(t)
	a.hostSchema.Store(6)
	if err := a.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["dummy-a","dummy-b"],"models":["unrelated-probe-model"]}`)); err != nil {
		t.Fatal(err)
	}
	if got := turnStateAfterAuth(t, a, "blocked", "dummy-a", "model", nil); !got.Terminate || !strings.Contains(string(got.ResponseBody), "turn_state_required") {
		t.Fatal("uncollected account escaped", got)
	}
	// Manual harvesting is a private local operation and does not use business admission.
	a.turnState.Before("learn", "dummy-a", "model", nil)
	token := rpcTurnStateTemplate()
	if err := a.turnState.Learn("learn", "dummy-a", "model", http.Header{turnstate.Header: {token}}); err != nil {
		t.Fatal(err)
	}
	if got := turnStateAfterAuth(t, a, "r1", "dummy-a", "model", nil); got.Terminate || got.Headers.Get(turnstate.Header) != token {
		t.Fatal("ready account failed", got)
	}
	if got := turnStateAfterAuth(t, a, "r1", "dummy-b", "model", nil); !got.Terminate {
		t.Fatal("retry escaped to missing state")
	}
	if len(a.accountRuntime.requests) != 0 {
		t.Fatal("refused fallback leaked concurrency")
	}
	if got := turnStateAfterAuth(t, a, "r2", "dummy-a", "other-model", nil); !got.Terminate {
		t.Fatal("template crossed model boundary")
	}
	for _, patch := range []string{`{"enabled":false}`, `{"enabled":true,"dry_run":true}`, `{"dry_run":false,"inject_mode":"replace-only"}`} {
		if err := a.turnState.Update([]byte(patch)); err != nil {
			t.Fatal(err)
		}
		if got := turnStateAfterAuth(t, a, "configuration", "dummy-a", "model", nil); !got.Terminate {
			t.Fatal("noninjecting configuration escaped", patch)
		}
	}
	if got := turnStateAfterAuth(t, a, "replace", "dummy-a", "model", http.Header{turnstate.Header: {strings.Repeat("x", 312)}}); got.Terminate || got.Headers.Get(turnstate.Header) != token {
		t.Fatal("replace-only eligible header refused", got)
	}
	if err := a.turnState.Clear("dummy-a", "model"); err != nil {
		t.Fatal(err)
	}
	if got := turnStateAfterAuth(t, a, "cleared", "dummy-a", "model", nil); !got.Terminate {
		t.Fatal("cleared state escaped")
	}
}

func TestAccountUsageUsesExactIndexAndPersistsFailure(t *testing.T) {
	a := newConfiguredApp(t)
	files := []hostAuthFile{{ID: "dummy-a", AuthIndex: "index-a", Type: "codex", Source: "file"}, {ID: "dummy-b", AuthIndex: "index-b", Type: "codex", Source: "file"}}
	a.SetHostCaller(func(method string, payload any) (json.RawMessage, error) {
		return mustMarshal(t, hostAuthListResponse{Files: files}), nil
	})
	for _, record := range []UsageRecord{{AuthIndex: "index-a", Provider: "codex", AuthType: "oauth", APIKey: testAPIKey, Model: "model", Detail: UsageDetail{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}, {AuthIndex: "index-b", Provider: "codex", AuthType: "oauth", APIKey: testAPIKey, Model: "model", Failed: true, Failure: UsageFailure{StatusCode: 429}}, {Provider: "codex", APIKey: testAPIKey, Model: "model", Detail: UsageDetail{InputTokens: 900}}} {
		if _, err := a.handleUsage(mustMarshal(t, record)); err != nil {
			t.Fatal(err)
		}
	}
	var response struct {
		Accounts []struct {
			AuthIndex string               `json:"auth_index"`
			Usage     billing.AccountUsage `json:"usage"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(a.getAccountRuntime(ManagementRequest{}).Body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Accounts) != 2 || response.Accounts[0].Usage.TotalTokens != 12 || response.Accounts[0].Usage.Successes != 1 || response.Accounts[1].Usage.Failures != 1 || response.Accounts[1].Usage.TotalTokens != 0 {
		t.Fatalf("usage attribution=%+v", response)
	}
}

func TestProtectedStateRejectsUnknownHostsAndLongLivedSessions(t *testing.T) {
	a := newConfiguredApp(t)
	if err := a.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","probe_accounts":["dummy-a"]}`)); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []uint32{0, 5} {
		a.hostSchema.Store(schema)
		got := turnStateAfterAuth(t, a, "unsupported", "dummy-a", "model", nil)
		if !got.Terminate || !strings.Contains(string(got.ResponseBody), "turn_state_host_unsupported") {
			t.Fatal("unsupported host accepted", schema, got)
		}
	}
	a.hostSchema.Store(6)
	for _, req := range []RequestInterceptRequest{
		{RequestID: "websocket", Headers: http.Header{"Upgrade": {"websocket"}}, Metadata: map[string]any{MetadataSelectedAuth: "dummy-a"}},
		{RequestID: "session", Metadata: map[string]any{MetadataSelectedAuth: "dummy-a", "execution_session_id": "dummy-session"}},
	} {
		got := a.enforceAccountRuntime(req)
		if !got.Terminate || !strings.Contains(string(got.ResponseBody), "turn_state_websocket_unsupported") {
			t.Fatal("long-lived session accepted", got)
		}
	}
	if len(a.accountRuntime.requests) != 0 {
		t.Fatal("unsupported requests acquired slots")
	}
}

func TestAccountRuntimeIncludesConfigCredentialsOnlyWithExactHostIndex(t *testing.T) {
	a := newConfiguredApp(t)
	a.SetHostCaller(func(string, any) (json.RawMessage, error) {
		return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{}}), nil
	})
	a.observeCandidates([]SchedulerAuthCandidate{{ID: "dummy-config", Provider: "codex", Attributes: map[string]string{"source": "config:codex[dummy]"}}})
	if body := string(a.getAccountRuntime(ManagementRequest{}).Body); !strings.Contains(body, billing.CredentialFingerprint("dummy-config")) || !strings.Contains(body, `"usage_available":false`) || !strings.Contains(body, `"usage":null`) {
		t.Fatal("unobserved config account cannot be configured or invented usage", body)
	}
	a.observeRouteCredential("request", "dummy-config", "index-config")
	var response struct {
		Accounts []struct {
			Ref   string `json:"credential_ref"`
			Index string `json:"auth_index"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(a.getAccountRuntime(ManagementRequest{}).Body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Accounts) != 1 || response.Accounts[0].Ref != billing.CredentialFingerprint("dummy-config") || response.Accounts[0].Index != "index-config" {
		t.Fatal("missing exact config account metadata", response)
	}
}

func TestCredentialRotationSharesInflightConcurrencyAndPersistsAliases(t *testing.T) {
	a := newConfiguredApp(t)
	r := a.accountRuntime
	refA, refB, refC := billing.CredentialFingerprint("dummy-rotation-a"), billing.CredentialFingerprint("dummy-rotation-b"), billing.CredentialFingerprint("dummy-rotation-c")
	response := a.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, map[string]any{"accounts": map[string]accountRuntimePolicy{refA: {1}, refB: {3}}})})
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	if !r.acquire(refA, "old-request") {
		t.Fatal("initial request refused")
	}
	if err := r.migrateCredentialRefs(map[string]string{refA: refB}); err != nil {
		t.Fatal(err)
	}
	if r.active[refB] != 1 || r.requests["old-request"] != refB || r.acquire(refB, "new-request") || r.acquire(refA, "old-alias-request") {
		t.Fatal("rotation bypassed the shared account limit")
	}
	r.release("old-request")
	if !r.acquire(refB, "new-request") {
		t.Fatal("old completion did not release the merged slot")
	}
	if err := r.migrateCredentialRefs(map[string]string{refB: refC}); err != nil {
		t.Fatal(err)
	}
	if err := r.migrateCredentialRefs(map[string]string{refB: refC}); err != nil {
		t.Fatal("repeat migration", err)
	}
	s := r.snapshot()
	if s.CredentialAliases[refA] != refC || s.CredentialAliases[refB] != refC || r.active[refC] != 1 || r.requests["new-request"] != refC {
		t.Fatal("rotation chain not compressed or lost active request")
	}
	loaded, err := loadAccountRuntimeSettings(r.path)
	if err != nil || loaded.CredentialAliases[refA] != refC || loaded.Accounts[refC].ConcurrencyLimit != 1 || loaded.Accounts[refA].ConcurrencyLimit != 1 {
		t.Fatal("aliases or strict policy did not persist", err)
	}
	response = a.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, map[string]any{"accounts": map[string]accountRuntimePolicy{refC: {2}}})})
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	if !r.acquire(refA, "second-request") || r.acquire(refB, "third-request") {
		t.Fatal("editing the new ref did not update the shared limit")
	}
	r.release("new-request")
	r.release("second-request")
	if len(r.active) != 0 {
		t.Fatal("rotated completion leaked slots")
	}
	loaded, err = loadAccountRuntimeSettings(r.path)
	if err != nil || loaded.Accounts[refA].ConcurrencyLimit != 2 || loaded.CredentialAliases[refB] != refC {
		t.Fatal("UI policy update lost aliases", err)
	}
	restarted := newAccountRuntime()
	restarted.settings = loaded
	if !restarted.acquire(refA, "restart-a") || !restarted.acquire(refC, "restart-c") || restarted.acquire(refB, "restart-b") {
		t.Fatal("restarted runtime did not share aliases")
	}
}

func TestCredentialRotationCannotCycleOverwriteAliasesOrPartiallySave(t *testing.T) {
	a := newConfiguredApp(t)
	r := a.accountRuntime
	refA, refB, refC := billing.CredentialFingerprint("dummy-a"), billing.CredentialFingerprint("dummy-b"), billing.CredentialFingerprint("dummy-c")
	r.settings.Accounts[refA] = accountRuntimePolicy{1}
	if !r.acquire(refA, "old") {
		t.Fatal("initial request refused")
	}
	if err := r.migrateCredentialRefs(map[string]string{refA: refB, refB: refA}); err == nil {
		t.Fatal("alias cycle accepted")
	}
	path := r.path
	r.path = t.TempDir()
	if err := r.migrateCredentialRefs(map[string]string{refA: refB}); err == nil {
		t.Fatal("failed save reported success")
	}
	if len(r.settings.CredentialAliases) != 0 || r.requests["old"] != refA || r.active[refA] != 1 {
		t.Fatal("failed save published partial aliases")
	}
	r.path = path
	if err := r.migrateCredentialRefs(map[string]string{refA: refB}); err != nil {
		t.Fatal(err)
	}
	response := a.setAccountRuntimeSettings(ManagementRequest{Body: mustMarshal(t, map[string]any{"credential_aliases": map[string]string{refA: refC}})})
	if response.StatusCode != 400 || r.settings.CredentialAliases[refA] != refB {
		t.Fatal("public API replaced internal rotation aliases")
	}
	response = a.setAccountRuntimeSettings(ManagementRequest{Body: []byte(`{"require_turn_state":false}`)})
	if response.StatusCode != 200 || r.settings.CredentialAliases[refA] != refB {
		t.Fatal("unrelated UI save removed aliases")
	}
}

func TestCredentialRotationMergesBothAlreadyActivePools(t *testing.T) {
	a := newConfiguredApp(t)
	r := a.accountRuntime
	refA, refB := billing.CredentialFingerprint("dummy-a"), billing.CredentialFingerprint("dummy-b")
	r.settings.Accounts[refA] = accountRuntimePolicy{2}
	r.settings.Accounts[refB] = accountRuntimePolicy{1}
	r.acquire(refA, "request-a")
	r.acquire(refB, "request-b")
	if err := r.migrateCredentialRefs(map[string]string{refA: refB}); err != nil {
		t.Fatal(err)
	}
	if r.active[refB] != 2 || r.acquire(refB, "third") {
		t.Fatal("active pools not merged")
	}
	r.release("request-a")
	if r.acquire(refA, "third") {
		t.Fatal("remaining old or new request lost during merge")
	}
	r.release("request-b")
	if !r.acquire(refA, "third") {
		t.Fatal("merged pool never released")
	}
}
