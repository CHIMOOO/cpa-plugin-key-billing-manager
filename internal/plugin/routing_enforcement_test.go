package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestLoadedAuthFileKeepsProviderGrantAfterInventoryRefresh(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}}})
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("host method = %q", method)
		}
		// CPA reports this shape for a still-loaded authentication file whose
		// path no longer exists. Inventory refresh must not change its group.
		return json.RawMessage(`{"files":[{"id":"dummy-loaded-codex","provider":"codex","source":"memory","path":"/auth/codex.json","email":"dummy@example.test"}]}`), nil
	})
	if err := app.refreshCredentialInventory(); err != nil {
		t.Fatal(err)
	}
	credentials := app.credentialInventory()
	if len(credentials) != 1 || credentials[0].Source != billing.CredentialSourceAuthFiles || credentials[0].DisplayName != "dummy@example.test" {
		t.Fatalf("loaded file classified incorrectly: %+v", credentials)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-loaded-codex"); response.Terminate {
		t.Fatalf("inventory grouping changed the provider grant: %+v", response)
	}
}

func TestAfterAuthEnforcesExactCredentialWithoutScheduler(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialIDs: []string{billing.CredentialFingerprint("dummy-allowed-file")}})
	for _, id := range []string{"dummy-allowed-file", "dummy-unselected-file", ""} {
		t.Run(id, func(t *testing.T) {
			response := afterAuthForTest(t, app, scope, id)
			wantDeny := id != "dummy-allowed-file"
			if response.Terminate != wantDeny || wantDeny && response.StatusCode != http.StatusForbidden {
				t.Fatalf("selected %q: %+v", id, response)
			}
		})
	}
	if err := app.store.SetAccessControl(billing.AccessControl{Enabled: false, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-unselected-file"); response.Terminate {
		t.Fatalf("disabled access control rejected: %+v", response)
	}
}

func TestAfterAuthProviderGrantRequiresExactHostClassification(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{CredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}}})
	app.observeCandidates([]SchedulerAuthCandidate{
		{ID: "dummy-codex-file", Provider: "codex", Attributes: map[string]string{"source_backend": "file"}},
		{ID: "dummy-codex-config", Provider: "codex", Attributes: map[string]string{"source_backend": "config"}},
		{ID: "dummy-claude-file", Provider: "claude", Attributes: map[string]string{"source_backend": "file"}},
	})
	for _, id := range []string{"dummy-codex-file", "dummy-codex-config", "dummy-claude-file", "dummy-unknown"} {
		response := afterAuthForTest(t, app, scope, id)
		if response.Terminate != (id != "dummy-codex-file") {
			t.Fatalf("selected %q: %+v", id, response)
		}
	}
	// A deny selector wins even when this exact file is allowlisted.
	if err := app.store.SetKeyRoutes(scope, billing.RouteBindings{RouteRule: billing.RouteRule{
		CredentialIDs:             []string{billing.CredentialFingerprint("dummy-codex-file")},
		DeniedCredentialProviders: []billing.CredentialProviderSelector{{Source: billing.CredentialSourceAuthFiles, Provider: "codex"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if response := afterAuthForTest(t, app, scope, "dummy-codex-file"); !response.Terminate {
		t.Fatal("provider deny was bypassed by exact allow")
	}
}

func TestAfterAuthEmptyWhitelistAndUngroupedDefaultDeny(t *testing.T) {
	app, scope := configuredRoutingApp(t, billing.RouteRule{})
	if response := afterAuthForTest(t, app, scope, "dummy-file"); !response.Terminate {
		t.Fatal("empty route granted access")
	}
	if err := app.store.SetAccessControl(billing.AccessControl{Enabled: true, DenyUngrouped: true}); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{scope, billing.CallerScope("dummy-new-key"), ""} {
		if response := afterAuthForTest(t, app, caller, "dummy-file"); !response.Terminate || response.StatusCode != http.StatusForbidden {
			t.Fatalf("unassigned caller accepted: %+v", response)
		}
	}
	raw, err := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
		Metadata: map[string]any{MetadataSource: SourcePluginHostModelCallback},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	if response.Terminate {
		t.Fatal("host helper should not use downstream permissions")
	}
}

func afterAuthForTest(t *testing.T, app *App, scope, credential string) RequestInterceptResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodRequestInterceptAfter, mustMarshal(t, RequestInterceptRequest{
		Model: "dummy-model", SourceFormat: "openai",
		Metadata: map[string]any{MetadataCallerScope: scope, MetadataSelectedAuth: credential},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response RequestInterceptResponse
	decodeResult(t, raw, &response)
	return response
}
