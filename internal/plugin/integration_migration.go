package plugin

import (
	"fmt"
	"net/http"

	"cpa-key-billing/internal/billing"
)

type integrationRefMigration struct {
	ID       string            `json:"id"`
	Mapping  map[string]string `json:"mapping"`
	Prepared bool              `json:"prepared"`
}

// integrationsMu serializes every caller. Failed host writes retain the new
// rotating token in memory for a repair attempt, never in plugin-owned files.
// Once host.auth.save succeeds, the pending record survives process restarts.
func (a *App) persistIntegrationRecoverably(account integrationAccount) error {
	if a.integrationUnsaved == nil {
		a.integrationUnsaved = map[string]integrationAccount{}
	}
	a.integrationUnsaved[account.ID] = account
	if err := a.persistIntegration(account); err != nil {
		return err
	}
	delete(a.integrationUnsaved, account.ID)
	return nil
}

func (a *App) integrationForMutation(id string) (integrationAccount, bool, error) {
	if account, ok := a.integrationUnsaved[id]; ok {
		return account, true, nil
	}
	account, err := a.getIntegration(id)
	return account, false, err
}

func integrationReplacementMapping(previous, next integrationAccount) map[string]string {
	oldChannels, _ := integrationChannels(previous)
	newChannels, _ := integrationChannels(next)
	oldByResource := map[string]integrationChannelDescriptor{}
	for _, channel := range oldChannels {
		oldByResource[channel.Resource] = channel
	}
	mapping := map[string]string{}
	for _, channel := range newChannels {
		old, exists := oldByResource[channel.Resource]
		if !exists || old.Config == nil || channel.Config == nil || old.Identity != channel.Identity {
			continue
		}
		oldID, newID := integrationExpectedCredentialID(previous, old), integrationExpectedCredentialID(next, channel)
		if oldID != newID {
			mapping[billing.CredentialFingerprint(oldID)] = billing.CredentialFingerprint(newID)
		}
	}
	return mapping
}

func (a *App) saveIntegrationReplacement(previous, account integrationAccount) (integrationAccount, error) {
	if previous.ID != "" {
		mapping := integrationReplacementMapping(previous, account)
		if len(mapping) > 0 {
			id, err := newIntegrationID()
			if err != nil {
				return account, err
			}
			account.PendingRefMigration = &integrationRefMigration{ID: id, Mapping: mapping}
		}
	}
	if err := a.persistIntegrationRecoverably(account); err != nil {
		return account, err
	}
	return a.prepareIntegrationReplacement(account)
}

func (a *App) migrateIntegrationBilling(mapping map[string]string, complete bool) error {
	a.routingMu.Lock()
	defer a.routingMu.Unlock()
	return a.migrateIntegrationBillingLocked(mapping, complete)
}

func (a *App) migrateIntegrationBillingLocked(mapping map[string]string, complete bool) error {
	previous := a.store.ConfigCredentials()
	if err := a.store.MigrateCredentialRefs(mapping, complete); err != nil {
		return err
	}
	a.replaceSyncedCredentials(previous, a.store.ConfigCredentials())
	return nil
}

func (a *App) prepareIntegrationReplacement(account integrationAccount) (integrationAccount, error) {
	if _, unsaved := a.integrationUnsaved[account.ID]; unsaved {
		if err := a.persistIntegrationRecoverably(account); err != nil {
			return account, err
		}
	}
	pending := account.PendingRefMigration
	if pending == nil || pending.Prepared {
		return account, nil
	}
	if !integrationIDPattern.MatchString(pending.ID) || len(pending.Mapping) == 0 {
		return account, fmt.Errorf("Invalid pending credential migration")
	}
	if err := a.migrateIntegrationBilling(pending.Mapping, false); err != nil {
		return account, fmt.Errorf("Credential permissions could not be prepared; repair this integration before publishing its channels")
	}
	if err := a.accountRuntime.migrateCredentialRefs(pending.Mapping); err != nil {
		return account, fmt.Errorf("Account concurrency could not be prepared; repair this integration before publishing its channels")
	}
	copyPending := *pending
	copyPending.Prepared = true
	account.PendingRefMigration = &copyPending
	if err := a.persistIntegrationRecoverably(account); err != nil {
		return account, err
	}
	return account, nil
}

func (a *App) commitIntegrationReplacement(req ManagementRequest) ManagementResponse {
	var input struct {
		ID          string `json:"id"`
		MigrationID string `json:"migration_id"`
	}
	if err := decodeStrict(req.Body, &input); err != nil {
		return errorResponse(err)
	}
	if !integrationIDPattern.MatchString(input.ID) || !integrationIDPattern.MatchString(input.MigrationID) {
		return integrationError(400, "Invalid credential migration identifier")
	}
	a.integrationsMu.Lock()
	defer a.integrationsMu.Unlock()
	account, _, err := a.integrationForMutation(input.ID)
	if err != nil {
		return integrationError(404, err.Error())
	}
	if account.PendingRefMigration == nil {
		if account.LastRefMigration != input.MigrationID {
			return integrationError(409, "This credential migration is no longer pending")
		}
		if _, unsaved := a.integrationUnsaved[account.ID]; unsaved {
			if err := a.persistIntegrationRecoverably(account); err != nil {
				return integrationError(502, err.Error())
			}
		}
		return integrationJSON(200, map[string]any{"committed": true, "account": a.integrationView(account)})
	}
	pending := account.PendingRefMigration
	if pending.ID != input.MigrationID || !pending.Prepared {
		return integrationError(409, "The credential migration is not prepared")
	}
	// The browser posts a complete /credentials/sync only after all native
	// channel writes. Exact new refs must be present and exact old refs absent.
	result := func() ManagementResponse {
		a.routingMu.Lock()
		defer a.routingMu.Unlock()
		configured := a.store.ConfigCredentials()
		for old, next := range pending.Mapping {
			if _, exists := configured[next]; !exists {
				return integrationError(409, "Publish every replacement channel and synchronize credentials before committing")
			}
			if _, exists := configured[old]; exists {
				return integrationError(409, "An old credential channel is still present; repair and synchronize all channels before committing")
			}
		}
		if err := a.migrateIntegrationBillingLocked(pending.Mapping, true); err != nil {
			return integrationError(500, "Credential permissions could not be committed; retry this migration")
		}
		return ManagementResponse{}
	}()
	if result.StatusCode != 0 {
		return result
	}
	account.LastRefMigration, account.PendingRefMigration = pending.ID, nil
	if err := a.persistIntegrationRecoverably(account); err != nil {
		return integrationError(http.StatusBadGateway, "Permissions migrated but the completion marker could not be saved; retry this migration")
	}
	return integrationJSON(200, map[string]any{"committed": true, "account": a.integrationView(account)})
}
