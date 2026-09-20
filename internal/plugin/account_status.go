package plugin

import (
	"path/filepath"
	"regexp"
	"strings"

	"cpa-key-billing/internal/billing"
)

type accountStatusPatch struct {
	Name      string `json:"name"`
	AuthIndex string `json:"auth_index"`
}

var nativeStatusAuthID = regexp.MustCompile(`^[a-z-]+:apikey:[a-f0-9]{12}(?:-[1-9][0-9]*)?$`)

type accountStatusInventory struct {
	byRef      map[string]hostAuthFile
	ambiguous  map[string]bool
	pathCounts map[string]int
}

func accountStatusIdentities(files []hostAuthFile) accountStatusInventory {
	result := accountStatusInventory{byRef: map[string]hostAuthFile{}, ambiguous: map[string]bool{}, pathCounts: map[string]int{}}
	indices := map[string][]string{}
	for _, file := range files {
		if file.ID == "" || file.AuthIndex == "" {
			continue
		}
		ref := billing.CredentialFingerprint(file.ID)
		if _, exists := result.byRef[ref]; exists {
			result.ambiguous[ref] = true
		}
		result.byRef[ref] = file
		indices[file.AuthIndex] = append(indices[file.AuthIndex], ref)
		if file.Path != "" {
			result.pathCounts[filepath.Clean(file.Path)]++
		}
	}
	for _, refs := range indices {
		if len(refs) > 1 {
			for _, ref := range refs {
				result.ambiguous[ref] = true
			}
		}
	}
	return result
}

func accountStatusProvider(file hostAuthFile) string {
	provider := strings.ToLower(strings.TrimSpace(file.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(file.Type))
	}
	return provider
}

// The existing host endpoint validates both identifiers before changing one
// account globally. Config API-key providers persist through excluded-models;
// OpenAI compatibility and arbitrary virtual providers have no such guarantee.
func (a *App) accountStatusDescriptor(file hostAuthFile, pathCounts map[string]int) (*accountStatusPatch, string) {
	if a.hostSchema.Load() < 6 {
		return nil, "host_unsupported"
	}
	if file.ID == "" || file.AuthIndex == "" {
		return nil, "unverified_identity"
	}
	provider := accountStatusProvider(file)
	if provider == "" || provider == integrationAuthType {
		return nil, "source_control_required"
	}
	switch credentialSourceFromHost(file) {
	case billing.CredentialSourceAuthFiles:
		// Plugin-expanded source files may represent multiple credentials.
		// Only expose ordinary physical-file identities, not a virtual child
		// or a source-file switch that would change unselected siblings.
		if file.Name == "" || filepath.Base(file.ID) != file.Name ||
			file.Path != "" && (filepath.Base(file.Path) != file.Name || pathCounts[filepath.Clean(file.Path)] > 1) {
			return nil, "source_control_required"
		}
	case billing.CredentialSourceAIProviders:
		switch provider {
		case "gemini", "gemini-interactions", "claude", "codex", "xai", "meta", "vertex":
			// This is a known host provider, and its identity must also have
			// the native StableID shape. Provider labels alone cannot turn an
			// arbitrary plugin-owned memory credential into a config entry.
			if !strings.HasPrefix(file.ID, provider+":apikey:") || !nativeStatusAuthID.MatchString(file.ID) {
				return nil, "unverified_identity"
			}
		default:
			return nil, "unsupported_native_provider"
		}
	default:
		return nil, "unverified_identity"
	}
	return &accountStatusPatch{Name: file.ID, AuthIndex: file.AuthIndex}, ""
}
