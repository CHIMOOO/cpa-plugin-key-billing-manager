package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func (d *DB) loadForwardedForBlock(state *billing.State) error {
	var (
		settings billing.ForwardedForBlock
		raw      string
	)
	if err := d.db.QueryRow("SELECT enabled, model_keywords_json, message FROM forwarded_for_block WHERE id = 1").
		Scan(&settings.Enabled, &raw, &settings.Message); err != nil {
		return fmt.Errorf("读取 X-Forwarded-For 拦截设置：%w", err)
	}
	if err := json.Unmarshal([]byte(raw), &settings.ModelKeywords); err != nil {
		return fmt.Errorf("解析 X-Forwarded-For 拦截关键词：%w", err)
	}
	normalized, err := billing.NormalizeForwardedForBlock(settings)
	if err != nil {
		return fmt.Errorf("读取 X-Forwarded-For 拦截设置：%w", err)
	}
	state.ForwardedForBlock = normalized
	return nil
}

func saveForwardedForBlock(tx *sql.Tx, settings billing.ForwardedForBlock) error {
	keywords := settings.ModelKeywords
	if keywords == nil {
		keywords = []string{}
	}
	raw, err := json.Marshal(keywords)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE forwarded_for_block SET enabled = ?, model_keywords_json = ?, message = ? WHERE id = 1",
		settings.Enabled, string(raw), settings.Message); err != nil {
		return fmt.Errorf("保存 X-Forwarded-For 拦截设置：%w", err)
	}
	return nil
}
