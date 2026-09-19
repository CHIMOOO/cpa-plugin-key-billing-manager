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
		routeIDs, err := json.Marshal(group.RouteIDs)
		if err != nil {
			return err
		}
		rule, err := json.Marshal(group.Rule)
		if err != nil {
			return fmt.Errorf("保存分组 %s：%w", group.ID, err)
		}
		if _, err := tx.Exec("INSERT INTO groups(position, id, name, disabled, route_ids_json, rule_json) VALUES (?, ?, ?, ?, ?, ?)",
			position, group.ID, group.Name, group.Disabled, string(routeIDs), string(rule)); err != nil {
			return fmt.Errorf("保存分组：%w", err)
		}
	}
	return nil
}

func (d *DB) loadGroups(state *billing.State) error {
	rows, err := d.db.Query("SELECT id, name, disabled, route_ids_json, rule_json FROM groups ORDER BY position")
	if err != nil {
		return fmt.Errorf("读取分组：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var group billing.KeyGroup
		var routeIDs, rule string
		if err := rows.Scan(&group.ID, &group.Name, &group.Disabled, &routeIDs, &rule); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(routeIDs), &group.RouteIDs); err != nil {
			return fmt.Errorf("解析分组路由：%w", err)
		}
		if err := json.Unmarshal([]byte(rule), &group.Rule); err != nil {
			return fmt.Errorf("解析分组 %s 的直接选择：%w", group.ID, err)
		}
		normalized, err := billing.NormalizeGroup(group)
		if err != nil {
			return fmt.Errorf("校验分组 %s：%w", group.ID, err)
		}
		state.Groups = append(state.Groups, normalized)
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
