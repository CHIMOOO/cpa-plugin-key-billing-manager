package billing

import (
	"strings"
	"time"
)

type Decision struct {
	Allowed         bool
	PlanID          string
	PlanName        string
	ModelUnresolved bool
	QuotaView
}

func (s *Store) Authorize(scope string, at time.Time) Decision {
	return s.authorize(scope, "", "", at, false)
}

// AuthorizeModel admits and activates only windows applicable to the stable
// billing model. Usage charges those same windows from the host's usage record.
func (s *Store) AuthorizeModel(scope, upstream, requested string, at time.Time) Decision {
	return s.authorize(scope, upstream, requested, at, true)
}

func (s *Store) authorize(scope, upstream, requested string, at time.Time, scoped bool) Decision {
	allowed := Decision{Allowed: true}
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return allowed
	}
	if at.IsZero() {
		at = s.Now()
	}
	decision := updateResult(s, func(state *State) (Decision, Changes) {
		key := state.Keys[scope]
		if key == nil || key.PlanID == "" {
			return allowed, Changes{}
		}
		touched := Changes{Keys: []string{scope}}
		plan, ok := state.FindPlan(key.PlanID)
		if !ok {
			key.PlanID, key.Cycles = "", nil
			return allowed, touched
		}
		if scoped {
			// CPA can resolve auto to a public alias before admission, then
			// report the credential's concrete model in usage.handle. No stable
			// request identity is exposed for assigning scoped usage across those
			// two calls, so require an explicit model instead of a free bucket.
			identity := requested
			if strings.TrimSpace(identity) == "" {
				identity = upstream
			}
			if strings.EqualFold(ModelWithoutThinkingSuffix(identity), "auto") {
				for _, window := range plan.Windows {
					if !window.Scope.IsZero() {
						return Decision{PlanID: plan.ID, PlanName: plan.Name, ModelUnresolved: true}, Changes{}
					}
				}
			}
			model := state.ResolveBillingModel(upstream, requested)
			plan = plan.forModel(model)
		}
		var changed Changes
		if settleExpiredCycles(key, at) {
			changed = touched
		}
		view := quotaView(key, plan, at)
		if scoped {
			view.applyMatchedWindows()
		}
		if !view.Blocked && activateCycles(key, plan, at) {
			changed = touched
			view = quotaView(key, plan, at)
			if scoped {
				view.applyMatchedWindows()
			}
		}
		return Decision{Allowed: !view.Blocked, PlanID: plan.ID, PlanName: plan.Name, QuotaView: view}, changed
	})
	if decision.Allowed {
		s.blocked.clear(scope)
	}
	return decision
}

func (v *QuotaView) applyMatchedWindows() {
	v.PartiallyBlocked = false
	v.Unlimited = len(v.Windows) == 0
	for _, window := range v.Windows {
		if window.Blocked {
			v.Blocked = true
			if window.EndAt.After(v.RetryAt) {
				v.RetryAt = window.EndAt
			}
		}
	}
}
