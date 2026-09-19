package plugin

import (
	"encoding/json"
	"testing"

	"cpa-key-billing/internal/billing"
)

func multiGroupRoutingApp(t *testing.T) (*App, string) {
	t.Helper()
	app := newConfiguredApp(t)
	const key = "dummy-multi-group-key"
	if _, err := app.store.SyncKeys([]string{key}, false); err != nil {
		t.Fatal(err)
	}
	scope := billing.CallerScope(key)
	for _, id := range []string{"account-a", "account-b"} {
		if _, err := app.store.CreateGroup(billing.KeyGroup{
			ID: id, Name: id, Rule: billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint(id)}},
		}, []string{scope}); err != nil {
			t.Fatal(err)
		}
	}
	return app, scope
}

func availabilityCandidate(id, status string) SchedulerAuthCandidate {
	return SchedulerAuthCandidate{ID: id, Provider: "codex", Status: status,
		Attributes: map[string]string{"source_backend": "file"}}
}

func TestMultiGroupSchedulerSkipsDisabledAccount(t *testing.T) {
	app, scope := multiGroupRoutingApp(t)
	disabled := availabilityCandidate("account-a", "disabled")
	disabled.Attributes["weight"] = "100"
	active := availabilityCandidate("account-b", "active")
	for _, candidates := range [][]SchedulerAuthCandidate{
		{disabled, active},
		{active, disabled, availabilityCandidate("outside", "active")},
	} {
		for range 12 {
			raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, candidates...)))
			if err != nil {
				t.Fatal(err)
			}
			var result SchedulerPickResponse
			decodeResult(t, raw, &result)
			if !result.Handled || result.AuthID != "account-b" {
				t.Fatalf("disabled account in one group hid another group's active account: %+v", result)
			}
		}
	}
}

func TestMultiGroupSchedulerDoesNotFallBackOutsideGroups(t *testing.T) {
	app, scope := multiGroupRoutingApp(t)
	for _, candidates := range [][]SchedulerAuthCandidate{
		{availabilityCandidate("account-a", "disabled"), availabilityCandidate("account-b", "disabled")},
		{availabilityCandidate("account-a", "disabled"), availabilityCandidate("outside", "active")},
	} {
		raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, candidates...)))
		if err != nil {
			t.Fatal(err)
		}
		var envelope Envelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != 503 {
			t.Fatalf("no usable authorized account should return 503: %+v", envelope)
		}
	}
}

func TestMultiGroupSchedulerTracksReenableAndGroupDenials(t *testing.T) {
	app, scope := multiGroupRoutingApp(t)
	ref := billing.CredentialFingerprint("account-a")
	app.credentials[ref] = credentialView{Ref: ref, Source: billing.CredentialSourceAuthFiles,
		Provider: "codex", Status: "disabled", Disabled: true, Unavailable: true}
	candidates := []SchedulerAuthCandidate{availabilityCandidate("account-a", "active"), availabilityCandidate("account-b", "disabled")}
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, candidates...)))
	if err != nil {
		t.Fatal(err)
	}
	var result SchedulerPickResponse
	decodeResult(t, raw, &result)
	if !result.Handled || result.AuthID != "account-a" {
		t.Fatalf("re-enabled account stayed blocked by stale inventory: %+v", result)
	}
	if item := app.credentials[ref]; item.Disabled || item.Unavailable || item.Status != "active" {
		t.Fatalf("live host status did not update stale inventory: %+v", item)
	}
	// Once both accounts are enabled, neither group's credential should hide
	// the other. An unrelated candidate still must not enter the merged pool.
	seen := make(map[string]int)
	for range 12 {
		raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope,
			availabilityCandidate("account-a", "active"), availabilityCandidate("account-b", "active"),
			availabilityCandidate("outside", "active"))))
		if err != nil {
			t.Fatal(err)
		}
		decodeResult(t, raw, &result)
		if !result.Handled {
			t.Fatal("merged group restriction was delegated to the unrestricted host pool")
		}
		seen[result.AuthID]++
	}
	if seen["account-a"] != 6 || seen["account-b"] != 6 || len(seen) != 2 {
		t.Fatalf("re-enabled accounts did not both participate across groups: %v", seen)
	}
	// Another group's deny still overrides a grant; enabling more groups must
	// never permit a globally disabled or explicitly denied account.
	rule := billing.RouteRule{CredentialIDs: []string{ref}, DeniedCredentialIDs: []string{billing.CredentialFingerprint("account-b")}}
	if _, err := app.store.UpdateGroup(billing.GroupPatch{ID: "account-a", Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	raw, err = app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope,
		availabilityCandidate("account-a", "disabled"), availabilityCandidate("account-b", "active"))))
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.OK || envelope.Error == nil || envelope.Error.HTTPStatus != 503 {
		t.Fatalf("group deny was lost in union: %+v, %v", envelope, err)
	}
}

func TestSchedulerDoesNotDelegateZeroWeightCandidates(t *testing.T) {
	app, scope := multiGroupRoutingApp(t)
	a, b := availabilityCandidate("account-a", "active"), availabilityCandidate("account-b", "active")
	a.Attributes["weight"] = "0"
	raw, err := app.HandleMethod(MethodSchedulerPick, mustMarshal(t, schedulerRequest(scope, a, b)))
	if err != nil {
		t.Fatal(err)
	}
	var result SchedulerPickResponse
	decodeResult(t, raw, &result)
	if !result.Handled || result.AuthID != "account-b" {
		t.Fatalf("zero weight candidate returned to host: %+v", result)
	}
}

func TestCandidateStatusDoesNotOverrideModelAvailability(t *testing.T) {
	rule := billing.RoutingDecision{RouteRule: billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("account-a")}}}
	// The host supplies candidates after its model-specific availability checks.
	// An error for one model must not lock the account out of every model.
	for _, status := range []string{"active", "error", "pending", ""} {
		if !candidateAllowed(availabilityCandidate("account-a", status), rule) {
			t.Fatalf("host-selected candidate excluded for status %q", status)
		}
	}
	if candidateAllowed(availabilityCandidate("account-a", " DISABLED "), rule) {
		t.Fatal("disabled status was not normalized")
	}
}

func TestCandidateObservationPreservesMissingStatus(t *testing.T) {
	app := newConfiguredApp(t)
	ref := billing.CredentialFingerprint("account-a")
	app.credentials[ref] = credentialView{Ref: ref, Source: billing.CredentialSourceAuthFiles,
		Provider: "codex", DisplayName: "dummy@example.test", Status: "disabled", Disabled: true, Unavailable: true}
	app.observeCandidates([]SchedulerAuthCandidate{{ID: "account-a"}})
	item := app.credentials[ref]
	if !item.Disabled || !item.Unavailable || item.Status != "disabled" || item.DisplayName != "dummy@example.test" {
		t.Fatalf("incomplete candidate erased known status: %+v", item)
	}
	app.observeCandidates([]SchedulerAuthCandidate{availabilityCandidate("account-a", "disabled")})
	if !app.credentials[ref].Disabled {
		t.Fatal("explicit disabled candidate lost its disabled flag")
	}
}

func TestAccountCredentialReportsDisabledAndUnavailableFlags(t *testing.T) {
	for _, test := range []struct {
		item credentialView
		want string
	}{
		{credentialView{Status: "active", Disabled: true}, "disabled"},
		{credentialView{Unavailable: true}, "unavailable"},
		{credentialView{Status: "error", Unavailable: true}, "error"},
		{credentialView{}, "active"},
	} {
		if got := accountCredential(test.item).Status; got != test.want {
			t.Fatalf("status = %q, want %q for %+v", got, test.want, test.item)
		}
	}
}
