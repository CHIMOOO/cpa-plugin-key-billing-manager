package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func (d *DB) loadAccessControl(state *billing.State) error {
	if err := d.db.QueryRow("SELECT enabled, deny_ungrouped FROM access_control WHERE id = 1").Scan(&state.AccessControl.Enabled, &state.AccessControl.DenyUngrouped); err != nil {
		return fmt.Errorf("读取访问控制：%w", err)
	}
	return nil
}

func replaceGroups(tx *sql.Tx, state *billing.State) error {
	if _, err := tx.Exec("DELETE FROM groups"); err != nil {
		return fmt.Errorf("保存分组：%w", err)
	}
	for position, group := range state.Groups {
		raw, err := json.Marshal(group.RouteIDs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO groups(position, id, name, route_ids_json) VALUES (?, ?, ?, ?)", position, group.ID, group.Name, string(raw)); err != nil {
			return fmt.Errorf("保存分组：%w", err)
		}
	}
	return nil
}

func (d *DB) loadGroups(state *billing.State) error {
	rows, err := d.db.Query("SELECT id, name, route_ids_json FROM groups ORDER BY position")
	if err != nil {
		return fmt.Errorf("读取分组：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var group billing.KeyGroup
		var raw string
		if err := rows.Scan(&group.ID, &group.Name, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &group.RouteIDs); err != nil {
			return fmt.Errorf("解析分组路由：%w", err)
		}
		group, err = billing.NormalizeGroup(group)
		if err != nil {
			return err
		}
		state.Groups = append(state.Groups, group)
	}
	return rows.Err()
}

func (d *DB) loadKeyGroups(state *billing.State) error {
	rows, err := d.db.Query("SELECT scope, group_id FROM key_groups ORDER BY scope, position")
	if err != nil {
		return fmt.Errorf("读取 API Key 分组：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, groupID string
		if err := rows.Scan(&scope, &groupID); err != nil {
			return err
		}
		key := state.Keys[scope]
		if key == nil {
			return fmt.Errorf("分组成员引用不存在的 API Key %q", scope)
		}
		// Keep missing group references so routing fails closed on corrupt data.
		key.GroupIDs = append(key.GroupIDs, groupID)
	}
	return rows.Err()
}
