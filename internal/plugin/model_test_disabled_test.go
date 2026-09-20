package plugin

import (
	"encoding/json"
	"errors"
	"testing"

	"cpa-key-billing/internal/billing"
)

func disabledModelTestFixture(t *testing.T, configured, statusOnly bool) (*App, hostAuthFile) {
	t.Helper()
	a, file, _ := modelTestFixture(t, configured)
	file.Disabled = !statusOnly
	file.Status = "disabled"
	caller := a.hostCaller
	a.SetHostCaller(func(method string, input any) (json.RawMessage, error) {
		switch method {
		case hostAuthList:
			return mustMarshal(t, hostAuthListResponse{Files: []hostAuthFile{file}}), nil
		case "host.auth.get_runtime":
			if input.(map[string]string)["auth_index"] == file.AuthIndex {
				return mustMarshal(t, map[string]any{"auth": file}), nil
			}
		}
		// The base fixture fails any attempt to save or enable credentials.
		return caller(method, input)
	})
	return a, file
}

func TestModelTestDisabledRuntimeAccountsStayDisabled(t *testing.T) {
	for _, configured := range []bool{false, true} {
		for _, statusOnly := range []bool{false, true} {
			a, file := disabledModelTestFixture(t, configured, statusOnly)
			ref := billing.CredentialFingerprint(file.ID)
			if configured {
				if err := a.store.SyncConfigCredentials(map[string]billing.ConfigCredential{ref: {Provider: file.Provider, Disabled: true}}); err != nil {
					t.Fatal(err)
				}
			}
			var inventory struct {
				Accounts []modelTestAccount `json:"accounts"`
			}
			response := a.getModelTests(ManagementRequest{})
			if response.StatusCode != 200 || json.Unmarshal(response.Body, &inventory) != nil || len(inventory.Accounts) != 1 || !inventory.Accounts[0].Disabled || !inventory.Accounts[0].Supported {
				t.Fatalf("disabled diagnostic account unavailable: %s", response.Body)
			}
			input := modelTestInput(file)
			if configured {
				disabled := true
				input.Config.Disabled = &disabled
			}
			a.accountRuntime.settings.Accounts[ref] = accountRuntimePolicy{ConcurrencyLimit: 1}
			prepared := preparedModelTest(t, a, input)
			if !prepared.Account.Disabled || !prepared.Account.Supported || prepared.Account.IdentitySource != "host-runtime" {
				t.Fatal("diagnostics changed or misrepresented the account status")
			}
			if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 429 {
				t.Fatal("disabled diagnostics bypassed the account concurrency limit")
			}
			response = a.completeModelTest(ManagementRequest{Body: mustMarshal(t, map[string]any{"test_id": prepared.TestID, "not_started": true})})
			if response.StatusCode != 200 || a.accountRuntime.active[ref] != 0 {
				t.Fatal("disabled diagnostic account lease was not released")
			}
			current, err := a.modelTestAccount(file.AuthIndex)
			if err != nil || current.Disabled != file.Disabled || current.Status != file.Status {
				t.Fatal("diagnostics changed the host account status")
			}
			if configured && !a.store.ConfigCredentials()[ref].Disabled {
				t.Fatal("diagnostics enabled the stored credential for normal routing")
			}
			if configured {
				input.Config.AuthID = "different-credential"
				if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 409 {
					t.Fatal("disabled diagnostic account bypassed exact identity validation")
				}
			}
		}
	}
}

func TestModelTestDisabledNativeSnapshotKeepsInventoryValidation(t *testing.T) {
	a, file, _ := modelTestFixture(t, true)
	a.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.get_runtime" {
			t.Fatalf("unexpected callback %s", method)
		}
		return nil, errors.New("configuration credentials are not listed")
	})
	input := modelTestInput(file)
	disabled := true
	input.Config.Provider = "openai-compatible-test"
	input.Config.AuthID = "openai-compatibility:test:0123456789ab"
	input.Config.AuthIndex = input.AuthIndex
	input.Config.CredentialRef = billing.CredentialFingerprint(input.Config.AuthID)
	input.Config.Disabled = &disabled
	ref := input.Config.CredentialRef
	if err := a.store.SyncConfigCredentials(map[string]billing.ConfigCredential{ref: {Provider: input.Config.Provider, Disabled: true}}); err != nil {
		t.Fatal(err)
	}
	prepared := preparedModelTest(t, a, input)
	if !prepared.Account.Disabled || !prepared.Account.Supported || prepared.Account.IdentitySource != "management-snapshot" || !a.store.ConfigCredentials()[ref].Disabled {
		t.Fatal("disabled native snapshot changed or misrepresented account status")
	}
	disabled = false
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 409 {
		t.Fatal("stale snapshot overrode the synchronized disabled status")
	}
	disabled = true
	a.credentialRefsByIndex[input.AuthIndex] = billing.CredentialFingerprint("conflicting-runtime-account")
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 409 {
		t.Fatal("disabled snapshot overrode an observed exact runtime identity")
	}
}

func TestModelTestDisabledDiagnosticsKeepRiskAndTurnStateProtection(t *testing.T) {
	a, file := disabledModelTestFixture(t, true, false)
	if response := a.setRiskConfig(ManagementRequest{Body: []byte(`{"enabled":true,"mode":"pre_block","blocked_keywords":["dummy-risk-word"]}`)}); response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	input := modelTestInput(file)
	input.Preset, input.Prompt = "free", "dummy-risk-word"
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, input)}); response.StatusCode != 403 || len(a.modelTests) != 0 {
		t.Fatal("disabled diagnostics bypassed configured content-risk protection")
	}
	a, file = disabledModelTestFixture(t, false, false)
	if err := a.turnState.Update([]byte(`{"enabled":true,"inject_mode":"always","models":["dummy-model"],"probe_accounts":["dummy-model-test-account"]}`)); err != nil {
		t.Fatal(err)
	}
	if response := a.prepareModelTest(ManagementRequest{Body: mustMarshal(t, modelTestInput(file))}); response.StatusCode != 409 || len(a.modelTests) != 0 {
		t.Fatal("disabled diagnostics bypassed required Turn State protection")
	}
	unsupported := modelTestAccountView(hostAuthFile{ID: "dummy-unsupported-file", AuthIndex: "dummy-index", Provider: "claude", Source: "file", Path: "dummy.json", Disabled: true})
	if unsupported.Supported {
		t.Fatal("disabled account was allowed without a supported native protocol")
	}
}
