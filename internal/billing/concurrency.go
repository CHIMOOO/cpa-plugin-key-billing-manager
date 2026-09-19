package billing

import "strings"

const MaxConcurrencyLimit = 10000

// SlotDecision reports the result of reserving one generation concurrency
// slot. Active is the number of slots already occupied when a request is
// refused, or the number occupied after a successful reservation.
type SlotDecision struct {
	Allowed  bool
	Acquired bool
	Limit    int
	Active   int
}

// AcquireSlot atomically checks and reserves one API-key-scoped generation
// slot. Every attributable generation is tracked even while its limit is zero,
// so lowering an unlimited key to a finite limit immediately accounts for
// requests that were already running.
func (s *Store) AcquireSlot(scope, requestID string) SlotDecision {
	decision := SlotDecision{Allowed: true}
	scope = strings.TrimSpace(scope)
	requestID = strings.TrimSpace(requestID)
	if scope == "" {
		return decision
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if key := s.state.Keys[scope]; key != nil {
		decision.Limit = key.ConcurrencyLimit
	}
	decision.Active = s.activeByScope[scope]
	if requestID == "" {
		// A finite limit cannot be enforced safely without an exact completion
		// correlation. Current compatible hosts always provide RequestID.
		decision.Allowed = decision.Limit <= 0
		return decision
	}
	if existingScope, exists := s.activeRequests[requestID]; exists {
		decision.Allowed = existingScope == scope
		// The original admission owns this reservation. Reporting a duplicate
		// as newly acquired would let a later admission check roll it back.
		decision.Acquired = false
		return decision
	}
	if decision.Limit > 0 && decision.Active >= decision.Limit {
		decision.Allowed = false
		return decision
	}

	s.activeRequests[requestID] = scope
	s.activeByScope[scope] = decision.Active + 1
	decision.Active++
	decision.Acquired = true
	return decision
}

// ReleaseSlot idempotently releases the slot reserved for requestID. Unknown
// and duplicate completion events are harmless.
func (s *Store) ReleaseSlot(requestID string) bool {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	scope, exists := s.activeRequests[requestID]
	if !exists {
		return false
	}
	delete(s.activeRequests, requestID)
	if active := s.activeByScope[scope]; active > 1 {
		s.activeByScope[scope] = active - 1
	} else {
		delete(s.activeByScope, scope)
	}
	return true
}

func (s *Store) SetConcurrencyLimit(scope string, limit int) error {
	scope = normalizeScope(scope)
	if scope == "" {
		return invalidf("API key identifier is required")
	}
	if limit < 0 || limit > MaxConcurrencyLimit {
		return invalidf("Concurrency limit must be an integer from 0 to %d", MaxConcurrencyLimit)
	}

	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.liveKey(scope)
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		if key.ConcurrencyLimit == limit {
			return struct{}{}, Changes{}, nil
		}
		key.ConcurrencyLimit = limit
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}
