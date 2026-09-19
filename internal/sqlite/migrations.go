package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
)

// v18 adds an enabled-by-default group switch without rewriting history.
func migrateToV18(tx *sql.Tx) error {
	if _, err := tx.Exec(groupDisabledSchema); err != nil {
		return fmt.Errorf("迁移分组启用状态：%w", err)
	}
	return nil
}

// v17 adds each group's direct model and credential selection, empty for every
// existing group, and leaves every other table untouched.
func migrateToV17(tx *sql.Tx) error {
	if _, err := tx.Exec(groupRuleSchema); err != nil {
		return fmt.Errorf("迁移分组直接选择：%w", err)
	}
	return nil
}

// v16 adds the X-Forwarded-For model block, disabled, and leaves every existing
// table untouched.
func migrateToV16(tx *sql.Tx) error {
	if _, err := tx.Exec(forwardedForBlockSchema); err != nil {
		return fmt.Errorf("迁移 X-Forwarded-For 拦截设置：%w", err)
	}
	return nil
}

// v15 adds group membership and persisted admission policy without rewriting
// keys, quotas, routes, or historical request records.
func migrateToV15(tx *sql.Tx) error {
	if _, err := tx.Exec(accessControlSchema); err != nil {
		return fmt.Errorf("迁移分组访问控制：%w", err)
	}
	return nil
}

func migrateToV14(tx *sql.Tx, version int) error {
	var steps []func(*sql.Tx) error
	switch version {
	case 10:
		steps = append(steps, migrateModelGroups, migrateSubscriptionPeriods)
	case 11:
		steps = append(steps, migrateLegacyRouteBindings, migrateSubscriptionPeriods)
	}
	if version <= 12 {
		steps = append(steps, migrateModelPricing, migrateQuotaWindows)
	}
	steps = append(steps, migrateCredentials)
	for _, step := range steps {
		if err := step(tx); err != nil {
			return err
		}
	}
	return nil
}

func migrateCredentials(tx *sql.Tx) error {
	// Do not discard unknown fields or ambiguous identities when dropping the table.
	var compatible, invalid bool
	if err := tx.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'credentials' AND type = 'table')
		AND count(*) = 4 AND sum(upper(type) = 'TEXT' AND hidden = 0 AND (
			(name = 'auth_index' AND pk = 1) OR
			(name IN ('provider', 'account', 'name') AND pk = 0)
		)) = 4 FROM pragma_table_xinfo('credentials')`).Scan(&compatible); err != nil {
		return fmt.Errorf("读取旧凭证表格式：%w", err)
	}
	if !compatible {
		return fmt.Errorf("旧凭证表格式不兼容")
	}
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM credentials
		WHERE typeof(auth_index) != 'text' OR typeof(provider) != 'text'
		OR typeof(account) != 'text' OR typeof(name) != 'text')`).Scan(&invalid); err != nil {
		return fmt.Errorf("迁移请求事件账户：%w", err)
	}
	if invalid {
		return fmt.Errorf("旧凭证表包含不兼容的数据")
	}
	_, err := tx.Exec(`
		ALTER TABLE request_events ADD COLUMN account TEXT NOT NULL DEFAULT '';
		UPDATE request_events AS r SET account = c.account,
			provider = coalesce(NULLIF(r.provider, ''), c.provider)
		FROM credentials c WHERE c.auth_index = r.auth_index;
		DROP TABLE credentials;
		CREATE TABLE config_credentials (
			ref         TEXT PRIMARY KEY,
			provider    TEXT NOT NULL,
			key_preview TEXT NOT NULL DEFAULT '',
			disabled    INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return fmt.Errorf("迁移凭证数据：%w", err)
	}
	return nil
}

func migrateModelPricing(tx *sql.Tx) error {
	_, err := tx.Exec(`
        ALTER TABLE prices RENAME COLUMN pattern TO model_id;
        CREATE UNIQUE INDEX prices_model_id ON prices(model_id COLLATE NOCASE);

        CREATE TABLE reference_prices_metadata (
            id                   INTEGER PRIMARY KEY CHECK (id = 1),
            source_url           TEXT    NOT NULL,
            content_hash         TEXT    NOT NULL DEFAULT '',
            version              INTEGER NOT NULL DEFAULT 0,
            fetched_at           INTEGER NOT NULL DEFAULT 0,
            model_count          INTEGER NOT NULL DEFAULT 0,
            last_attempt_at      INTEGER NOT NULL DEFAULT 0,
            retry_after          INTEGER NOT NULL DEFAULT 0,
            last_error           TEXT    NOT NULL DEFAULT '',
            consecutive_failures INTEGER NOT NULL DEFAULT 0
        );

        CREATE TABLE reference_prices (
            provider_id  TEXT    NOT NULL,
            model_id     TEXT    NOT NULL,
            match_key    TEXT    NOT NULL,
            is_canonical INTEGER NOT NULL,
            rates_json   TEXT,
            PRIMARY KEY (provider_id, model_id)
        );
    `)
	return err
}

// migrateModelGroups converts the v10 model-access tables into route bindings.
func migrateModelGroups(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE routes (
			position INTEGER PRIMARY KEY,
			id TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			rule_json TEXT NOT NULL DEFAULT '{}'
		)`); err != nil {
		return fmt.Errorf("创建路由规则表：%w", err)
	}
	if _, err := tx.Exec("ALTER TABLE api_keys ADD COLUMN route_bindings_json TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return fmt.Errorf("添加 API Key 路由绑定：%w", err)
	}
	type legacyGroup struct {
		oldID, id, name string
		models          []string
	}
	groups := []legacyGroup{}
	usedRouteIDs := map[string]struct{}{}
	rows, err := tx.Query("SELECT id,name FROM model_groups ORDER BY position")
	if err != nil {
		return fmt.Errorf("读取旧模型分组：%w", err)
	}
	for rows.Next() {
		var group legacyGroup
		if err := rows.Scan(&group.oldID, &group.name); err != nil {
			rows.Close()
			return fmt.Errorf("读取旧模型分组：%w", err)
		}
		group.id = strings.TrimSpace(group.oldID)
		if _, exists := usedRouteIDs[group.id]; exists {
			return fmt.Errorf("旧模型分组 ID %q 迁移后冲突", group.oldID)
		}
		usedRouteIDs[group.id] = struct{}{}
		groups = append(groups, group)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取旧模型分组：%w", err)
	}
	byOld := make(map[string]int, len(groups))
	for i := range groups {
		byOld[groups[i].oldID] = i
	}
	members, err := tx.Query("SELECT group_id,model FROM model_group_models ORDER BY group_id,position")
	if err != nil {
		return fmt.Errorf("读取旧模型分组成员：%w", err)
	}
	for members.Next() {
		var id, model string
		if err := members.Scan(&id, &model); err != nil {
			members.Close()
			return fmt.Errorf("读取旧模型分组成员：%w", err)
		}
		if i, ok := byOld[id]; ok {
			groups[i].models = append(groups[i].models, model)
		}
	}
	if err := members.Close(); err != nil {
		return err
	}
	if err := members.Err(); err != nil {
		return fmt.Errorf("读取旧模型分组成员：%w", err)
	}
	for i, group := range groups {
		route, err := billing.NormalizeRoute(billing.Route{ID: group.id, Name: group.name, Rule: billing.RouteRule{Models: group.models}})
		if err != nil {
			return fmt.Errorf("校验旧模型分组 %s：%w", group.oldID, err)
		}
		groups[i].id = route.ID
		groups[i].models = route.Rule.Models
		raw, err := json.Marshal(route.Rule)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO routes(position,id,name,rule_json) VALUES(?,?,?,?)", i, route.ID, route.Name, string(raw)); err != nil {
			return fmt.Errorf("迁移模型分组 %s：%w", group.oldID, err)
		}
	}

	bindingsByScope := make(map[string]billing.RouteBindings)
	groupRows, err := tx.Query("SELECT scope,group_id FROM key_model_groups ORDER BY scope,position")
	if err != nil {
		return fmt.Errorf("读取旧模型分组绑定：%w", err)
	}
	for groupRows.Next() {
		var scope, id string
		if err := groupRows.Scan(&scope, &id); err != nil {
			groupRows.Close()
			return fmt.Errorf("读取旧模型分组绑定：%w", err)
		}
		if i, ok := byOld[id]; ok && len(groups[i].models) > 0 {
			bindings := bindingsByScope[scope]
			bindings.RouteIDs = append(bindings.RouteIDs, groups[i].id)
			bindingsByScope[scope] = bindings
		}
	}
	if err := groupRows.Close(); err != nil {
		return err
	}
	if err := groupRows.Err(); err != nil {
		return fmt.Errorf("读取旧模型分组绑定：%w", err)
	}

	modelRows, err := tx.Query("SELECT scope,model FROM key_allowed_models ORDER BY scope,position")
	if err != nil {
		return fmt.Errorf("读取旧模型绑定：%w", err)
	}
	for modelRows.Next() {
		var scope, model string
		if err := modelRows.Scan(&scope, &model); err != nil {
			modelRows.Close()
			return fmt.Errorf("读取旧模型绑定：%w", err)
		}
		bindings := bindingsByScope[scope]
		bindings.Models = append(bindings.Models, model)
		bindingsByScope[scope] = bindings
	}
	if err := modelRows.Close(); err != nil {
		return err
	}
	if err := modelRows.Err(); err != nil {
		return fmt.Errorf("读取旧模型绑定：%w", err)
	}

	for scope, bindings := range bindingsByScope {
		bindings, err = billing.NormalizeRouteBindings(bindings)
		if err != nil {
			return fmt.Errorf("校验 API Key %s 的迁移绑定：%w", scope, err)
		}
		raw, err := json.Marshal(bindings)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE api_keys SET route_bindings_json=? WHERE scope=?", string(raw), scope); err != nil {
			return fmt.Errorf("迁移 API Key %s：%w", scope, err)
		}
	}
	for _, table := range []string{"key_model_groups", "key_allowed_models", "model_group_models", "model_groups"} {
		if _, err := tx.Exec("DROP TABLE " + table); err != nil {
			return fmt.Errorf("删除旧表 %s：%w", table, err)
		}
	}
	return nil
}

func migrateLegacyRouteBindings(tx *sql.Tx) error {
	updates := map[string]string{}
	rows, err := tx.Query("SELECT scope,route_bindings_json FROM api_keys")
	if err != nil {
		return fmt.Errorf("读取旧 API Key 路由绑定：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, raw string
		if err = rows.Scan(&scope, &raw); err != nil {
			return fmt.Errorf("读取旧 API Key 路由绑定：%w", err)
		}
		var legacy []struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		if err = json.Unmarshal([]byte(raw), &legacy); err != nil {
			return fmt.Errorf("读取 API Key %s 的旧路由绑定：%w", scope, err)
		}
		var bindings billing.RouteBindings
		for _, binding := range legacy {
			switch binding.Kind {
			case "route":
				bindings.RouteIDs = append(bindings.RouteIDs, binding.Value)
			case "model":
				bindings.Models = append(bindings.Models, binding.Value)
			case "credential":
				bindings.CredentialIDs = append(bindings.CredentialIDs, binding.Value)
			case "credential_provider":
				source, provider, ok := strings.Cut(binding.Value, "\x00")
				if !ok {
					return fmt.Errorf("API Key %s 的旧凭证类别绑定无效", scope)
				}
				bindings.CredentialProviders = append(bindings.CredentialProviders,
					billing.CredentialProviderSelector{Source: source, Provider: provider})
			default:
				return fmt.Errorf("API Key %s 的旧路由绑定类型 %q 无效", scope, binding.Kind)
			}
		}
		bindings, err = billing.NormalizeRouteBindings(bindings)
		if err != nil {
			return fmt.Errorf("校验 API Key %s 的旧路由绑定：%w", scope, err)
		}
		converted, err := json.Marshal(bindings)
		if err != nil {
			return err
		}
		updates[scope] = string(converted)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("读取旧 API Key 路由绑定：%w", err)
	}
	for scope, raw := range updates {
		if _, err = tx.Exec("UPDATE api_keys SET route_bindings_json=? WHERE scope=?", raw, scope); err != nil {
			return fmt.Errorf("迁移 API Key %s 的路由绑定：%w", scope, err)
		}
	}
	if _, err = tx.Exec("DELETE FROM routes WHERE id = 'system:all'"); err != nil {
		return fmt.Errorf("清理旧路由规则：%w", err)
	}

	return nil
}

func migrateSubscriptionPeriods(tx *sql.Tx) error {
	var id, kind string
	var seconds int64
	err := tx.QueryRow(`
		SELECT id, period_kind, period_seconds FROM plans
		WHERE period_kind NOT IN ('daily', 'weekly', 'monthly', 'custom', 'never')
		   OR (period_kind = 'custom' AND (period_seconds <= 0 OR period_seconds > ?))
		LIMIT 1`, int64(math.MaxInt64)/int64(time.Second)).Scan(&id, &kind, &seconds)
	if err == nil {
		return fmt.Errorf("订阅计划 %s 的旧周期无效：%s (%d 秒)", id, kind, seconds)
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("检查旧订阅计划：%w", err)
	}
	if _, err = tx.Exec(`
		UPDATE plans SET period_seconds =
			CASE period_kind
				WHEN 'daily' THEN 86400
				WHEN 'weekly' THEN 604800
				WHEN 'monthly' THEN 2592000
				WHEN 'custom' THEN period_seconds
				WHEN 'never' THEN 0
			END
	`); err != nil {
		return fmt.Errorf("迁移订阅计划：%w", err)
	}
	if _, err = tx.Exec("ALTER TABLE plans DROP COLUMN period_kind"); err != nil {
		return fmt.Errorf("删除旧订阅周期类型：%w", err)
	}
	return nil
}

func migrateQuotaWindows(tx *sql.Tx) error {
	rows, err := tx.Query("SELECT id, name, amount_usd, period_seconds FROM plans")
	if err != nil {
		return err
	}
	plans := make(map[string]billing.Plan)
	convertedPeriods := make(map[string]bool)
	for rows.Next() {
		var plan billing.Plan
		window := billing.QuotaWindow{ID: "default", Name: "默认额度"}
		if err := rows.Scan(&plan.ID, &plan.Name, &window.AmountUSD, &window.PeriodSeconds); err != nil {
			rows.Close()
			return err
		}
		if window.PeriodSeconds == 0 {
			window.PeriodSeconds = 365 * 24 * 60 * 60
			convertedPeriods[plan.ID] = true
		}
		plan.Windows = []billing.QuotaWindow{window}
		if err := plan.Validate(); err != nil {
			rows.Close()
			return err
		}
		plans[plan.ID] = plan
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.Query("SELECT scope, plan_id, cycle_plan_id, cycle_start_at, cycle_end_at, cycle_spent_usd FROM api_keys")
	if err != nil {
		return err
	}
	cycles := make(map[string]string)
	for rows.Next() {
		var scope string
		var key billing.KeyState
		var cycle billing.QuotaCycle
		var start, end int64
		if err := rows.Scan(&scope, &key.PlanID, &cycle.PlanID, &start, &end, &cycle.SpentUSD); err != nil {
			rows.Close()
			return err
		}
		cycle.StartAt, cycle.EndAt = timeAt(start), timeAt(end)
		if convertedPeriods[key.PlanID] && !cycle.StartAt.IsZero() && cycle.EndAt.IsZero() {
			cycle.EndAt = cycle.StartAt.Add(365 * 24 * time.Hour)
		}
		key.Cycles = map[string]billing.QuotaCycle{}
		if cycle != (billing.QuotaCycle{}) {
			key.Cycles["default"] = cycle
		}
		if err := key.ValidateCycles(plans[key.PlanID]); err != nil {
			rows.Close()
			return fmt.Errorf("迁移额度周期：%w", err)
		}
		raw, err := json.Marshal(key.Cycles)
		if err != nil {
			rows.Close()
			return err
		}
		cycles[scope] = string(raw)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE plans ADD COLUMN windows_json TEXT NOT NULL DEFAULT '[]';
        ALTER TABLE api_keys ADD COLUMN cycles_json TEXT NOT NULL DEFAULT '{}';`); err != nil {
		return err
	}
	for id, plan := range plans {
		raw, err := json.Marshal(plan.Windows)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE plans SET windows_json=? WHERE id=?", string(raw), id); err != nil {
			return err
		}
	}
	for scope, raw := range cycles {
		if _, err := tx.Exec("UPDATE api_keys SET cycles_json=? WHERE scope=?", raw, scope); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`ALTER TABLE plans DROP COLUMN amount_usd;
        ALTER TABLE plans DROP COLUMN period_seconds;
        ALTER TABLE api_keys DROP COLUMN cycle_plan_id;
        ALTER TABLE api_keys DROP COLUMN cycle_start_at;
        ALTER TABLE api_keys DROP COLUMN cycle_end_at;
        ALTER TABLE api_keys DROP COLUMN cycle_spent_usd;`)
	return err
}
