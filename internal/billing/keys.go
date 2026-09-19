package billing

import (
	"sort"
	"strings"
	"time"
)

type KeyView struct {
	Scope     string    `json:"scope"`
	Preview   string    `json:"preview,omitempty"`
	Label     string    `json:"label,omitempty"`
	InConfig  bool      `json:"in_config"`
	DeletedAt time.Time `json:"deleted_at,omitzero"`

	PlanID             string        `json:"plan_id,omitempty"`
	PlanName           string        `json:"plan_name,omitempty"`
	ConcurrencyLimit   int           `json:"concurrency_limit"`
	CurrentConcurrency int           `json:"current_concurrency"`
	RouteBindings      RouteBindings `json:"route_bindings"`
	GroupIDs           []string      `json:"group_ids"`
	QuotaView
}

// Settle expired cycles before displaying quota status for active keys.
func (s *Store) KeyViews() []KeyView {
	now := s.Now()
	return updateResult(s, func(state *State) ([]KeyView, Changes) {
		var settled []string
		plans := make(map[string]Plan, len(state.Plans))
		for _, plan := range state.Plans {
			plans[plan.ID] = plan
		}
		views := make([]KeyView, 0, len(state.Keys))
		for scope, key := range state.Keys {
			if key == nil {
				continue
			}
			if key.DeletedAt.IsZero() && settleKeyPlan(key, plans[key.PlanID], now) {
				settled = append(settled, scope)
			}
			views = append(views, keyView(scope, key, plans[key.PlanID], s.activeByScope[scope], now))
		}
		sortKeyViews(views)
		return views, Changes{Keys: settled}
	})
}

func keyView(scope string, key *KeyState, plan Plan, currentConcurrency int, now time.Time) KeyView {
	return KeyView{
		Scope:              scope,
		Preview:            key.Preview,
		Label:              key.Label,
		InConfig:           key.InConfig,
		DeletedAt:          key.DeletedAt,
		PlanID:             key.PlanID,
		PlanName:           plan.Name,
		QuotaView:          quotaView(key, plan, now),
		ConcurrencyLimit:   key.ConcurrencyLimit,
		CurrentConcurrency: currentConcurrency,
		RouteBindings:      key.RouteBindings.clone(),
		GroupIDs:           append([]string{}, key.GroupIDs...),
	}
}

func settleKeyPlan(key *KeyState, plan Plan, now time.Time) bool {
	if key.PlanID == "" {
		return false
	}
	if plan.ID == key.PlanID {
		return settleExpiredCycles(key, now)
	}
	key.PlanID = ""
	key.Cycles = nil
	return true
}

func (s *Store) KeyViewForScope(scope string) (KeyView, bool) {
	scope = normalizeScope(scope)
	if scope == "" {
		return KeyView{}, false
	}
	type result struct {
		view KeyView
		ok   bool
	}
	current := updateResult(s, func(state *State) (result, Changes) {
		key := state.Keys[scope]
		if key == nil || !key.DeletedAt.IsZero() {
			return result{}, Changes{}
		}
		plan, _ := state.FindPlan(key.PlanID)
		now := s.Now()
		changed := Changes{}
		if settleKeyPlan(key, plan, now) {
			changed.Keys = []string{scope}
		}
		return result{view: keyView(scope, key, plan, s.activeByScope[scope], now), ok: true}, changed
	})
	return current.view, current.ok
}

func sortKeyViews(views []KeyView) {
	sort.Slice(views, func(i, j int) bool {
		if views[i].Blocked != views[j].Blocked {
			return views[i].Blocked
		}
		left, right := strings.ToLower(views[i].Label), strings.ToLower(views[j].Label)
		if left != right {
			return left < right
		}
		return views[i].Scope < views[j].Scope
	})
}

func (s *Store) BindKey(scope, planID string) error {
	scope = normalizeScope(scope)
	planID = strings.TrimSpace(planID)
	if scope == "" || planID == "" {
		return invalidf("API key identifier and subscription plan ID are required")
	}
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		plan, exists := state.FindPlan(planID)
		if !exists {
			return struct{}{}, Changes{}, notFoundf("Subscription plan %q does not exist", planID)
		}
		key := state.liveKey(scope)
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		if key.PlanID == plan.ID {
			return struct{}{}, Changes{}, nil
		}
		key.Cycles = nil
		key.PlanID = plan.ID
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}

func (s *Store) UnbindKey(scope string) error {
	scope = normalizeScope(scope)
	if scope == "" {
		return invalidf("API key identifier is required")
	}
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.liveKey(scope)
		if key == nil || key.PlanID == "" {
			return struct{}{}, Changes{}, nil
		}
		key.PlanID = ""
		key.Cycles = nil
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}

type ResetRequest struct {
	Mode   string   `json:"mode"`
	Scopes []string `json:"scopes,omitempty"`
}

type ResetResult struct {
	Keys    int `json:"keys"`
	Windows int `json:"windows"`
}

func (s *Store) ResetQuota(req ResetRequest) (ResetResult, error) {
	req.Scopes = normalizeScopes(req.Scopes)
	if req.Mode == "global" {
		if len(req.Scopes) != 0 {
			return ResetResult{}, invalidf("A reset of all quotas cannot specify individual keys")
		}
	} else if req.Mode == "all" {
		if len(req.Scopes) == 0 {
			return ResetResult{}, invalidf("Select the API keys to reset")
		}
	} else {
		return ResetResult{}, invalidf("Invalid quota reset mode")
	}
	return editConfiguration(s, func(state *State) (ResetResult, Changes, error) {
		scopes := req.Scopes
		if req.Mode == "global" {
			for scope, key := range state.Keys {
				if key != nil && key.DeletedAt.IsZero() && key.PlanID != "" {
					scopes = append(scopes, scope)
				}
			}
		}
		for _, scope := range scopes {
			key := state.liveKey(scope)
			if key == nil || key.PlanID == "" {
				return ResetResult{}, Changes{}, invalidf("The API key does not exist or has no subscription plan")
			}
		}
		result := ResetResult{}
		var changed []string
		for _, scope := range scopes {
			key := state.Keys[scope]
			count := len(key.Cycles)
			key.Cycles = nil
			if count > 0 {
				changed = append(changed, scope)
				result.Keys++
				result.Windows += count
			}
		}
		return result, Changes{Keys: changed}, nil
	})
}

func (s *Store) SetLabel(scope, label string) error {
	scope = normalizeScope(scope)
	if scope == "" {
		return invalidf("API key identifier is required")
	}
	label = strings.TrimSpace(label)

	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.Keys[scope]
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		key.Label = label
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}

// Deleted keys retain their settings but cannot be rebound or reset.
func (s *State) liveKey(scope string) *KeyState {
	key := s.Keys[scope]
	if key == nil || !key.DeletedAt.IsZero() {
		return nil
	}
	return key
}

type SyncResult struct {
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

func (s *State) ensureKey(scope, preview string) *KeyState {
	if scope == "" {
		return nil
	}
	preview = strings.TrimSpace(preview)
	if preview == "" {
		preview = UnknownKeyPreview
	}
	key := s.Keys[scope]
	if key == nil {
		key = &KeyState{Preview: preview}
		s.Keys[scope] = key
	} else if key.Preview == "" || preview != UnknownKeyPreview && key.Preview != preview {
		key.Preview = preview
	}
	return key
}

// SyncKeys reconciles the tracked keys with the list CPA currently holds.
//
// Plaintext keys are discarded after producing a scope hash and masked preview.
// A key missing from the list is marked deleted rather than dropped, and only if
// an earlier sync saw it, so principals from other access providers survive.
// allowEmpty prevents an accidental empty push from marking every synchronized
// record deleted.
//
// Restoring a key preserves its bindings and spent quota. Normal cycle expiry
// and explicit resets still apply.
func (s *Store) SyncKeys(keys []string, allowEmpty bool) (SyncResult, error) {
	scopes := make(map[string]string, len(keys))
	for _, key := range keys {
		scope := CallerScope(key)
		if scope == "" {
			continue
		}
		scopes[scope] = PreviewKey(key)
	}
	if len(scopes) == 0 && !allowEmpty {
		return SyncResult{}, invalidf("The API key list is empty; pass allow_empty to clear it")
	}

	now := s.Now()

	return editConfiguration(s, func(state *State) (SyncResult, Changes, error) {
		var result SyncResult
		changed := false
		for scope, preview := range scopes {
			if key := state.Keys[scope]; key == nil || key.Preview != preview {
				changed = true
			}
			key := state.ensureKey(scope, preview)
			key.Preview = preview
			if !key.InConfig {
				result.Added++
			}
			if !key.DeletedAt.IsZero() {
				key.DeletedAt = time.Time{}
				changed = true
			}
			if !key.InConfig {
				key.InConfig = true
				changed = true
			}
		}
		for scope, key := range state.Keys {
			if _, listed := scopes[scope]; listed || key == nil {
				continue
			}
			if key.InConfig {
				key.InConfig = false
				key.DeletedAt = now
				result.Deleted++
				changed = true
			}
		}
		if !changed {
			return result, Changes{}, nil
		}
		return result, Changes{AllKeys: true}, nil
	})
}

// Scopes are hex digests, so case folding is safe for hand-typed input.
func normalizeScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func normalizeScopes(scopes []string) []string {
	lowered := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		lowered = append(lowered, normalizeScope(scope))
	}
	return dedupe(lowered)
}

// dedupe drops blanks and repeats from an operator's list. Values are compared
// case-insensitively but kept in the spelling they arrived in, because model
// names and identifiers are read back on screen.
func dedupe(values []string) []string {
	var deduped []string
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		lowered := strings.ToLower(value)
		if _, exists := seen[lowered]; exists {
			continue
		}
		seen[lowered] = struct{}{}
		deduped = append(deduped, value)
	}
	return deduped
}
