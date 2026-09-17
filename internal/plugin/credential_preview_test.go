package plugin

import (
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestCredentialSyncPreservesMaskedPreview(t *testing.T) {
	for _, key := range []string{
		"a", "ab", "abc", "abcd", "sk-short-key", "甲乙丙丁",
		"sk-dummy-abcdefghijklmnopqrstuvwxyz-12345678",
	} {
		t.Run(key, func(t *testing.T) {
			app := newConfiguredApp(t)
			ref := billing.CredentialFingerprint("dummy-preview-credential")
			want := billing.PreviewKey(key)
			for _, value := range []string{key, want} {
				callOK(t, app, http.MethodPost, routeCredentialsSync, nil, map[string]any{
					"credentials": []map[string]any{{"ref": ref, "provider": "codex", "display_name": value}},
				}, http.StatusOK, nil)
				inventory := app.credentialInventory()
				if len(inventory) != 1 || inventory[0].DisplayName != want || app.store.ConfigCredentials()[ref].KeyPreview != want {
					t.Fatalf("sync changed the key preview: %+v", inventory)
				}
			}
		})
	}
}
