package sqlite

import (
	"database/sql"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func replaceConfigCredentials(tx *sql.Tx, state *billing.State) error {
	if _, err := tx.Exec("DELETE FROM config_credentials"); err != nil {
		return fmt.Errorf("Save configured credentials: %w", err)
	}
	for ref, credential := range state.ConfigCredentials {
		if _, err := tx.Exec("INSERT INTO config_credentials(ref, provider, key_preview, disabled) VALUES(?, ?, ?, ?)",
			ref, credential.Provider, credential.KeyPreview, credential.Disabled); err != nil {
			return fmt.Errorf("Save configured credentials: %w", err)
		}
	}
	return nil
}

func (d *DB) loadConfigCredentials(state *billing.State) error {
	rows, err := d.db.Query("SELECT ref, provider, key_preview, disabled FROM config_credentials")
	if err != nil {
		return fmt.Errorf("Read configured credentials: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		var credential billing.ConfigCredential
		if err := rows.Scan(&ref, &credential.Provider, &credential.KeyPreview, &credential.Disabled); err != nil {
			return fmt.Errorf("Read configured credentials: %w", err)
		}
		state.ConfigCredentials[ref] = credential
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("Read configured credentials: %w", err)
	}
	return nil
}
