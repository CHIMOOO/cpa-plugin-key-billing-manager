package plugin

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"cpa-key-billing/internal/billing"
)

// Compact protocol snapshot from https://models.dev/api.json, providers
// opencode and opencode-go, retrieved 2026-09-20. Per-model provider.npm
// overrides the provider default; unknown models are never guessed from names.
//
//go:embed integration_opencode_protocols.json
var integrationOpenCodeProtocolJSON []byte

var integrationOpenCodeProtocols = func() map[string]map[string]string {
	var providers map[string]map[string]string
	if err := json.Unmarshal(integrationOpenCodeProtocolJSON, &providers); err != nil {
		panic("invalid embedded integration model protocols")
	}
	return providers
}()

type integrationUnsupportedModel struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type integrationChannelDescriptor struct {
	Resource      string         `json:"resource"`
	IdentityField string         `json:"identity_field"`
	Identity      string         `json:"identity"`
	Protocol      string         `json:"protocol"`
	Models        []string       `json:"models"`
	Config        map[string]any `json:"config"`
}

type integrationChannelView struct {
	Resource      string   `json:"resource"`
	IdentityField string   `json:"identity_field"`
	Identity      string   `json:"identity"`
	Protocol      string   `json:"protocol"`
	Models        []string `json:"models"`
}

func integrationProtocol(account integrationAccount, id string) string {
	if account.Kind != "opencode-go" && account.Kind != "opencode-zen" {
		return "chat"
	}
	provider := "opencode"
	if account.Kind == "opencode-go" {
		provider = "opencode-go"
	}
	return integrationOpenCodeProtocols[provider][id]
}

func integrationChannels(account integrationAccount) ([]integrationChannelDescriptor, []integrationUnsupportedModel) {
	view := integrationViewOf(account)
	byProtocol := map[string][]string{"chat": {}, "responses": {}, "anthropic": {}}
	unsupported := []integrationUnsupportedModel{}
	for _, id := range account.Models {
		protocol := integrationProtocol(account, id)
		if _, ok := byProtocol[protocol]; ok {
			byProtocol[protocol] = append(byProtocol[protocol], id)
		} else {
			reason := "The upstream protocol for this model is not in the verified catalog; it was not published."
			if protocol == "google" {
				reason = "This Google model uses an endpoint unsupported by the host provider configuration; it was not published."
			}
			unsupported = append(unsupported, integrationUnsupportedModel{ID: id, Reason: reason})
		}
	}
	headers := map[string]string{}
	for name, values := range integrationAccountHeaders(account) {
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	protocols := []string{"chat"}
	if account.Kind == "opencode-go" || account.Kind == "opencode-zen" {
		protocols = append(protocols, "responses", "anthropic")
	}
	descriptors := []integrationChannelDescriptor{}
	for _, protocol := range protocols {
		desc := integrationChannelDescriptor{Resource: "openai-compatibility", IdentityField: "name", Identity: view.ChannelName, Protocol: protocol, Models: byProtocol[protocol]}
		baseURL := view.BaseURL
		if protocol == "responses" {
			desc.Resource, desc.IdentityField, desc.Identity = "codex-api-key", "prefix", view.ChannelName+"-responses"
		}
		if protocol == "anthropic" {
			desc.Resource, desc.IdentityField, desc.Identity = "claude-api-key", "prefix", view.ChannelName+"-anthropic"
			baseURL = strings.TrimSuffix(baseURL, "/v1")
		}
		if account.APIKey != "" && !account.Deleted && len(desc.Models) > 0 {
			models := []map[string]any{}
			for _, id := range desc.Models {
				model := map[string]any{"name": id, "alias": id}
				if protocol != "chat" {
					model["is-compat"] = true
				}
				models = append(models, model)
			}
			desc.Config = map[string]any{desc.IdentityField: desc.Identity, "base-url": baseURL, "headers": headers, "models": models}
			if protocol == "chat" {
				desc.Config["api-key-entries"] = []map[string]string{{"api-key": account.APIKey}}
			} else {
				desc.Config["api-key"] = account.APIKey
			}
		}
		descriptors = append(descriptors, desc)
	}
	return descriptors, unsupported
}

func integrationExpectedCredentialID(account integrationAccount, descriptor integrationChannelDescriptor) string {
	if descriptor.Config == nil {
		return ""
	}
	baseURL, _ := descriptor.Config["base-url"].(string)
	kind := "openai-compatibility:" + descriptor.Identity
	parts := []string{account.APIKey, baseURL, ""}
	if descriptor.Protocol != "chat" {
		kind = "codex:apikey"
		if descriptor.Protocol == "anthropic" {
			kind = "claude:apikey"
		}
		headers, _ := descriptor.Config["headers"].(map[string]string)
		keys := make([]string, 0, len(headers))
		for key := range headers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var formatted strings.Builder
		for _, key := range keys {
			formatted.WriteString(key)
			formatted.WriteByte(0)
			formatted.WriteString(headers[key])
			formatted.WriteByte(0)
		}
		parts = append(parts, descriptor.Identity, formatted.String())
	}
	var input strings.Builder
	input.WriteString(kind)
	for _, part := range parts {
		input.WriteByte(0)
		input.WriteString(strings.TrimSpace(part))
	}
	digest := sha256.Sum256([]byte(input.String()))
	return kind + ":" + hex.EncodeToString(digest[:])[:12]
}

func (a *App) integrationView(account integrationAccount) integrationView {
	view := integrationViewOf(account)
	descriptors, unsupported := integrationChannels(account)
	if account.PendingRefMigration != nil {
		view.PendingMigration = account.PendingRefMigration.ID
	}
	view.UnsupportedModels = unsupported
	view.Channels = []integrationChannelView{}
	view.CredentialRefs = []string{}
	view.ClientModels = []string{}
	files, _ := a.listHostAuthFiles()
	for _, descriptor := range descriptors {
		view.Channels = append(view.Channels, integrationChannelView{Resource: descriptor.Resource, IdentityField: descriptor.IdentityField, Identity: descriptor.Identity, Protocol: descriptor.Protocol, Models: descriptor.Models})
		if descriptor.Config == nil {
			continue
		}
		for _, model := range descriptor.Models {
			if descriptor.Protocol == "chat" {
				view.ClientModels = append(view.ClientModels, model)
			} else {
				view.ClientModels = append(view.ClientModels, descriptor.Identity+"/"+model)
			}
		}
		expected := integrationExpectedCredentialID(account, descriptor)
		ref := billing.CredentialFingerprint(expected)
		found := false
		for _, file := range files {
			if file.ID == expected && credentialSourceFromHost(file) == billing.CredentialSourceAIProviders {
				found = true
				break
			}
		}
		if !found {
			// Older hosts omit config-backed auths from host.auth.list. The
			// management browser synchronizes their exact stable fingerprints.
			provider := "openai-compatible-" + descriptor.Identity
			if descriptor.Protocol == "responses" {
				provider = "codex"
			}
			if descriptor.Protocol == "anthropic" {
				provider = "claude"
			}
			a.routingMu.Lock()
			credential, exists := a.credentials[ref]
			a.routingMu.Unlock()
			found = exists && credential.Source == billing.CredentialSourceAIProviders && credential.Provider == provider
		}
		if found {
			view.CredentialRefs = append(view.CredentialRefs, ref)
		}
	}
	return view
}

func (a *App) integrationPayload(account integrationAccount) map[string]any {
	channels, _ := integrationChannels(account)
	var legacyChannel any
	for _, channel := range channels {
		if channel.Protocol == "chat" {
			legacyChannel = channel.Config
		}
	}
	payload := map[string]any{"account": a.integrationView(account), "channels": channels, "channel": legacyChannel}
	if account.PendingRefMigration != nil {
		payload["migration_id"] = account.PendingRefMigration.ID
	}
	return payload
}
