package billing

import "time"

// AccountUsage aggregates only persisted usage.handle records with an exact
// host AuthIndex. Historical unclassified token detail is not recoverable.
type AccountUsage struct {
	AuthIndex         string    `json:"auth_index"`
	Requests          int64     `json:"requests"`
	Successes         int64     `json:"successes"`
	Failures          int64     `json:"failures"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	TotalTokens       int64     `json:"total_tokens"`
	AmountUSD         float64   `json:"amount_usd"`
	IncompleteRecords int64     `json:"incomplete_records"`
	LastUsedAt        time.Time `json:"last_used_at,omitzero"`
}

func (s *Store) AccountUsage() ([]AccountUsage, error) {
	return withRepository(s, func(repo Repository) ([]AccountUsage, error) {
		if reader, ok := repo.(interface {
			AccountUsage(time.Time) ([]AccountUsage, error)
		}); ok {
			return reader.AccountUsage(s.Now().Add(-RequestEventRetention))
		}
		view, err := repo.RequestEvents(RequestEventQuery{}, s.Now().Add(-RequestEventRetention))
		if err != nil {
			return nil, err
		}
		byIndex := map[string]*AccountUsage{}
		for _, event := range view.Entries {
			if event.AuthIndex == "" {
				continue
			}
			u := byIndex[event.AuthIndex]
			if u == nil {
				u = &AccountUsage{AuthIndex: event.AuthIndex}
				byIndex[event.AuthIndex] = u
			}
			u.Requests++
			if event.Failed {
				u.Failures++
			} else {
				u.Successes++
			}
			u.InputTokens += event.Cost.UncachedInputTokens + event.Cost.CacheReadTokens + event.Cost.CacheWriteTokens
			u.OutputTokens += event.Cost.BilledOutputTokens
			u.TotalTokens = u.InputTokens + u.OutputTokens
			u.AmountUSD += event.Cost.TotalUSD
			if event.AccountingQuality != TokenAccountingComplete {
				u.IncompleteRecords++
			}
			if event.At.After(u.LastUsedAt) {
				u.LastUsedAt = event.At
			}
		}
		result := []AccountUsage{}
		for _, usage := range byIndex {
			result = append(result, *usage)
		}
		return result, nil
	})
}
