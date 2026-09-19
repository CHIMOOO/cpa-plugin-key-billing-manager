package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
)

func (d *DB) AppendPluginLog(entry billing.PluginLog, cutoff time.Time) error {
	return d.transact(func(tx *sql.Tx) error {
		if _, errInsert := tx.Exec(
			"INSERT INTO plugin_logs (at, level, message) VALUES (?, ?, ?)",
			nanos(entry.At), string(entry.Level), entry.Message); errInsert != nil {
			return fmt.Errorf("Write plugin log: %w", errInsert)
		}
		return prunePluginLogs(tx.Exec, cutoff)
	})
}

func prunePluginLogs(exec execer, cutoff time.Time) error {
	if cutoff.IsZero() {
		return nil
	}
	if _, errPrune := exec("DELETE FROM plugin_logs WHERE at < ?", nanos(cutoff)); errPrune != nil {
		return fmt.Errorf("Clean up expired plugin logs: %w", errPrune)
	}
	return nil
}

func (d *DB) PluginLogsPage(query billing.PluginLogQuery) (billing.PluginLogPage, error) {
	counts, err := d.db.Query("SELECT level, count(*) FROM plugin_logs WHERE at >= ? GROUP BY level", nanos(query.Since))
	if err != nil {
		return billing.PluginLogPage{}, fmt.Errorf("Count plugin logs: %w", err)
	}
	page := billing.PluginLogPage{Entries: []billing.PluginLog{}, LevelCounts: map[billing.PluginLogLevel]int{}}
	for counts.Next() {
		var level billing.PluginLogLevel
		var count int
		if err := counts.Scan(&level, &count); err != nil {
			counts.Close()
			return billing.PluginLogPage{}, fmt.Errorf("Count plugin logs: %w", err)
		}
		page.LevelCounts[level] = count
	}
	if err := counts.Close(); err != nil {
		return billing.PluginLogPage{}, fmt.Errorf("Count plugin logs: %w", err)
	}
	if err := counts.Err(); err != nil {
		return billing.PluginLogPage{}, fmt.Errorf("Count plugin logs: %w", err)
	}

	where := []string{"at >= ?"}
	args := []any{nanos(query.Since)}
	if query.BeforeID > 0 {
		where = append(where, "id < ?")
		args = append(args, query.BeforeID)
	}
	if len(query.Levels) > 0 {
		marks := make([]string, len(query.Levels))
		for i, level := range query.Levels {
			marks[i] = "?"
			args = append(args, string(level))
		}
		where = append(where, "level IN ("+strings.Join(marks, ",")+")")
	}
	args = append(args, query.Limit+1)
	rows, err := d.db.Query("SELECT id,at,level,message FROM plugin_logs WHERE "+strings.Join(where, " AND ")+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return billing.PluginLogPage{}, fmt.Errorf("Read plugin logs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry billing.PluginLog
		var at int64
		var level string
		if err := rows.Scan(&entry.ID, &at, &level, &entry.Message); err != nil {
			return billing.PluginLogPage{}, fmt.Errorf("Read plugin logs: %w", err)
		}
		entry.At = timeAt(at)
		entry.Level = billing.PluginLogLevel(level)
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return billing.PluginLogPage{}, fmt.Errorf("Read plugin logs: %w", err)
	}
	if len(page.Entries) > query.Limit {
		page.Entries = page.Entries[:query.Limit]
		page.NextBeforeID = page.Entries[len(page.Entries)-1].ID
	}
	return page, nil
}

func (d *DB) ClearPluginLogs() (int, error) {
	result, err := d.db.Exec("DELETE FROM plugin_logs")
	if err != nil {
		return 0, fmt.Errorf("Clear plugin logs: %w", err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("Clear plugin logs: %w", err)
	}
	return int(cleared), nil
}
