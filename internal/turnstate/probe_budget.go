package turnstate

import (
	"sort"
	"time"

	"cpa-key-billing/internal/messages"
)

const (
	maxProbeUsageBuckets = 3601
	// Larger than every supported limit. Saturating arithmetic cannot make a
	// full budget eligible, even after implausibly large request volumes.
	maxProbeUsageCount = 1_000_000_000
)

// ProbeBudget reports the shared rolling-hour reservation budget. Limit zero
// means unlimited, with Remaining zero and no ResumesAt. Reservations count
// even when credential loading, transport, or response validation later fails.
type ProbeBudget struct {
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	Remaining int       `json:"remaining"`
	Exhausted bool      `json:"exhausted"`
	ResumesAt time.Time `json:"resumes_at,omitzero"`
}

// Entries within one wall-clock second share the latest reservation time.
// This bounds normal storage to 3601 entries without discarding counts, and
// may defer their release by less than one second. They are sorted by At.
// Both limited and unlimited collection maintain the same durable history.
type probeUsage struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

func addProbeUsageCount(a, b int) int {
	if a >= maxProbeUsageCount-b {
		return maxProbeUsageCount
	}
	return a + b
}

func validateProbeUsage(usage []probeUsage) error {
	if len(usage) > maxProbeUsageBuckets {
		return messages.Errorf("Invalid turn-state state file")
	}
	for index, entry := range usage {
		if entry.At.IsZero() || entry.Count < 1 || entry.Count > maxProbeUsageCount || (index > 0 && !entry.At.After(usage[index-1].At)) {
			return messages.Errorf("Invalid turn-state state file")
		}
	}
	return nil
}

func pruneProbeUsage(usage []probeUsage, now time.Time) []probeUsage {
	cutoff := now.Add(-time.Hour)
	first := sort.Search(len(usage), func(index int) bool { return usage[index].At.After(cutoff) })
	return usage[first:]
}

func reserveProbeUsage(usage []probeUsage, now time.Time) []probeUsage {
	usage = pruneProbeUsage(usage, now)
	now = now.UTC()
	// A backward clock jump must not lose reservations made at later wall-clock
	// times. Insert in order, then compact conservatively if required.
	index := sort.Search(len(usage), func(index int) bool { return usage[index].At.Unix() >= now.Unix() })
	if index < len(usage) && usage[index].At.Unix() == now.Unix() {
		usage[index].Count = addProbeUsageCount(usage[index].Count, 1)
		if now.After(usage[index].At) {
			usage[index].At = now
		}
		return usage
	}
	usage = append(usage, probeUsage{})
	copy(usage[index+1:], usage[index:])
	usage[index] = probeUsage{At: now, Count: 1}
	if len(usage) > maxProbeUsageBuckets {
		// Clock rollback can leave more than an hour of future entries. Charge
		// the oldest segment until the next segment expires; never drop it.
		usage[1].Count = addProbeUsageCount(usage[0].Count, usage[1].Count)
		usage = usage[1:]
	}
	return usage
}

func probeBudget(state diskState, now time.Time) ProbeBudget {
	budget := ProbeBudget{Limit: state.Config.ProbeHourlyLimit}
	usage := pruneProbeUsage(state.ProbeUsage, now)
	for _, entry := range usage {
		budget.Used = addProbeUsageCount(budget.Used, entry.Count)
	}
	if budget.Limit == 0 {
		return budget
	}
	budget.Remaining = max(0, budget.Limit-budget.Used)
	budget.Exhausted = budget.Used >= budget.Limit
	if budget.Exhausted {
		// Find the earliest expiry that actually admits one new request. The
		// limit may have been reduced below the already reserved usage.
		remaining := 0
		for index := len(usage) - 1; index >= 0; index-- {
			remaining = addProbeUsageCount(remaining, usage[index].Count)
			if remaining >= budget.Limit {
				budget.ResumesAt = usage[index].At.Add(time.Hour)
				break
			}
		}
	}
	return budget
}
