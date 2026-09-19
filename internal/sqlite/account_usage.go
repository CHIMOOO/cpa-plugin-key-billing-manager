package sqlite

import (
	"fmt"
	"time"

	"cpa-key-billing/internal/billing"
)

func (d *DB) AccountUsage(since time.Time) ([]billing.AccountUsage, error) {
	rows, err := d.db.Query(`SELECT auth_index, count(*), coalesce(sum(failed = 0),0), coalesce(sum(failed != 0),0),
		coalesce(sum(uncached_input_tokens + cache_read_tokens + cache_write_tokens),0), coalesce(sum(billed_output_tokens),0),
		coalesce(sum(total_usd),0), coalesce(sum(accounting_quality != 'complete'),0), max(at)
		FROM request_events WHERE at >= ? AND auth_index != '' GROUP BY auth_index ORDER BY auth_index`, nanos(since))
	if err != nil {
		return nil, fmt.Errorf("Read account usage: %w", err)
	}
	defer rows.Close()
	result := []billing.AccountUsage{}
	for rows.Next() {
		var u billing.AccountUsage
		var at int64
		if err := rows.Scan(&u.AuthIndex, &u.Requests, &u.Successes, &u.Failures, &u.InputTokens, &u.OutputTokens, &u.AmountUSD, &u.IncompleteRecords, &at); err != nil {
			return nil, err
		}
		u.TotalTokens = u.InputTokens + u.OutputTokens
		u.LastUsedAt = time.Unix(0, at).UTC()
		result = append(result, u)
	}
	return result, rows.Err()
}
