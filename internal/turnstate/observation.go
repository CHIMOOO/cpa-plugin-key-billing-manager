package turnstate

import (
	"sort"
	"time"
)

const (
	observationBucketLimit = 256
	observationEventLimit  = 100
	observationHourLimit   = 48
)

// ObservationCounts describes response header lengths, not request outcomes,
// usage, or model quality. "Native" means this plugin did not write a header;
// the client may still have supplied its own state.
type ObservationCounts struct {
	NativeMatch     uint64 `json:"native_match"`
	NativeReplace   uint64 `json:"native_replace"`
	NativeOther     uint64 `json:"native_other"`
	InjectedSilent  uint64 `json:"injected_silent"`
	InjectedMatch   uint64 `json:"injected_match"`
	InjectedReplace uint64 `json:"injected_replace"`
	InjectedOther   uint64 `json:"injected_other"`
}

// ObservationEvent deliberately excludes raw state values and request bodies.
// Kind reports only a match against the configured lengths at observation time.
type ObservationEvent struct {
	At       time.Time `json:"at"`
	Account  string    `json:"account"`
	Model    string    `json:"model"`
	Injected bool      `json:"injected"`
	Length   int       `json:"length"`
	Kind     string    `json:"kind"`
}

type ObservationBucket struct {
	Account    string            `json:"account"`
	Model      string            `json:"model"`
	Counts     ObservationCounts `json:"counts"`
	Recent24h  ObservationCounts `json:"recent_24h"`
	Last       ObservationEvent  `json:"last"`
	LastSigned *ObservationEvent `json:"last_signed,omitempty"`
}

// ObservationStatus is volatile diagnostics. It is reset on a process restart
// or data-path switch; ordinary settings edits and template clearing retain it.
type ObservationStatus struct {
	Since   time.Time           `json:"since,omitzero"`
	Buckets []ObservationBucket `json:"buckets"`
	Events  []ObservationEvent  `json:"events"`
}

type observationHour struct {
	At     time.Time
	Counts ObservationCounts
}

type observationBucket struct {
	View  ObservationBucket
	Hours []observationHour
}

type observationState struct {
	Since   time.Time
	Buckets map[string]*observationBucket
	Events  []ObservationEvent
}

func addObservationCount(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func (c *ObservationCounts) add(other ObservationCounts) {
	c.NativeMatch = addObservationCount(c.NativeMatch, other.NativeMatch)
	c.NativeReplace = addObservationCount(c.NativeReplace, other.NativeReplace)
	c.NativeOther = addObservationCount(c.NativeOther, other.NativeOther)
	c.InjectedSilent = addObservationCount(c.InjectedSilent, other.InjectedSilent)
	c.InjectedMatch = addObservationCount(c.InjectedMatch, other.InjectedMatch)
	c.InjectedReplace = addObservationCount(c.InjectedReplace, other.InjectedReplace)
	c.InjectedOther = addObservationCount(c.InjectedOther, other.InjectedOther)
}

func observationIncrement(injected bool, kind string) ObservationCounts {
	var counts ObservationCounts
	if injected {
		switch kind {
		case "silent":
			counts.InjectedSilent = 1
		case "match":
			counts.InjectedMatch = 1
		case "replace":
			counts.InjectedReplace = 1
		default:
			counts.InjectedOther = 1
		}
	} else {
		switch kind {
		case "match":
			counts.NativeMatch = 1
		case "replace":
			counts.NativeReplace = 1
		default:
			counts.NativeOther = 1
		}
	}
	return counts
}

// recordObservationLocked runs only with an exact request-side account/model
// relay. It performs no I/O and does not start background work.
func (m *Manager) recordObservationLocked(p pending, length int, now time.Time) {
	if length == 0 && !p.Wrote {
		return
	}
	kind := "other"
	switch length {
	case 0:
		kind = "silent"
	case m.state.Config.TemplateLength:
		kind = "match"
	case m.state.Config.ReplaceLength:
		kind = "replace"
	}
	o := &m.observations
	if o.Buckets == nil {
		o.Buckets = make(map[string]*observationBucket)
	}
	if o.Since.IsZero() {
		o.Since = now.UTC()
	}
	k := key(p.Account, p.Model)
	b := o.Buckets[k]
	if b == nil {
		if len(o.Buckets) >= observationBucketLimit {
			oldest := ""
			for candidate, existing := range o.Buckets {
				if oldest == "" || existing.View.Last.At.Before(o.Buckets[oldest].View.Last.At) ||
					(existing.View.Last.At.Equal(o.Buckets[oldest].View.Last.At) && candidate < oldest) {
					oldest = candidate
				}
			}
			delete(o.Buckets, oldest)
		}
		b = &observationBucket{View: ObservationBucket{Account: p.Account, Model: p.Model}}
		o.Buckets[k] = b
	}
	event := ObservationEvent{At: now.UTC(), Account: p.Account, Model: p.Model, Injected: p.Wrote, Length: length, Kind: kind}
	b.View.Last = event
	if length > 0 {
		signed := event
		b.View.LastSigned = &signed
	}
	increment := observationIncrement(p.Wrote, kind)
	b.View.Counts.add(increment)
	hour := now.UTC().Truncate(time.Hour)
	cutoff := hour.Add(-time.Duration(observationHourLimit-1) * time.Hour)
	slot := -1
	kept := b.Hours[:0]
	for _, existing := range b.Hours {
		if existing.At.Before(cutoff) || existing.At.After(hour) {
			continue
		}
		if existing.At.Equal(hour) {
			slot = len(kept)
		}
		kept = append(kept, existing)
	}
	b.Hours = kept
	if slot < 0 {
		b.Hours = append(b.Hours, observationHour{At: hour})
		slot = len(b.Hours) - 1
	}
	b.Hours[slot].Counts.add(increment)
	if len(o.Events) == observationEventLimit {
		copy(o.Events, o.Events[1:])
		o.Events[len(o.Events)-1] = event
	} else {
		o.Events = append(o.Events, event)
	}
}

func (m *Manager) observationStatusLocked(now time.Time) ObservationStatus {
	o := m.observations
	status := ObservationStatus{Since: o.Since, Buckets: []ObservationBucket{}, Events: []ObservationEvent{}}
	hour := now.UTC().Truncate(time.Hour)
	// 24 UTC hourly buckets including the current partial hour. This is an
	// hourly diagnostic summary, not an exact rolling request-rate metric.
	cutoff := hour.Add(-23 * time.Hour)
	for _, b := range o.Buckets {
		view := b.View
		if view.LastSigned != nil {
			signed := *view.LastSigned
			view.LastSigned = &signed
		}
		for _, slot := range b.Hours {
			if !slot.At.Before(cutoff) && !slot.At.After(hour) {
				view.Recent24h.add(slot.Counts)
			}
		}
		status.Buckets = append(status.Buckets, view)
	}
	sort.Slice(status.Buckets, func(i, j int) bool {
		return key(status.Buckets[i].Account, status.Buckets[i].Model) < key(status.Buckets[j].Account, status.Buckets[j].Model)
	})
	for i := len(o.Events) - 1; i >= 0; i-- {
		status.Events = append(status.Events, o.Events[i])
	}
	return status
}
