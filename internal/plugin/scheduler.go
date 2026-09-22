package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"cpa-key-billing/internal/billing"
)

const maxPoolsPerKey = 256
const maxCredentialsPerPool = 1024
const noRoutedCredentialMessage = "No available upstream credentials match the routing rules"

type subsetScheduleState struct {
	Current map[string]int64
	Weights map[string]int64
}
type subsetScheduler struct {
	mu   sync.Mutex
	keys map[string]map[string]*subsetScheduleState
}

func candidateWeight(candidate SchedulerAuthCandidate) int64 {
	raw := strings.TrimSpace(candidate.Attributes["weight"])
	if raw == "" {
		return 1
	}
	weight, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || weight <= 0 {
		return 0
	}
	return weight
}

func routingPoolKey(model string, decision billing.RoutingDecision) string {
	// Keep round-robin progress separate per model; this does not change which
	// credentials the key is allowed to use.
	policy := decision.RouteRule
	policy.Models, policy.DeniedModels = nil, nil
	var constraint *billing.RouteRule
	if decision.CredentialConstraint != nil {
		copyRule := *decision.CredentialConstraint
		copyRule.Models, copyRule.DeniedModels = nil, nil
		constraint = &copyRule
	}
	raw, _ := json.Marshal(struct {
		Rule       billing.RouteRule
		Constraint *billing.RouteRule
	}{policy, constraint})
	sum := sha256.Sum256(raw)
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + hex.EncodeToString(sum[:])
}

func (s *subsetScheduler) pick(scope, pool string, candidates []SchedulerAuthCandidate) string {
	positive := make([]SchedulerAuthCandidate, 0, len(candidates))
	weights := make(map[string]int64, len(candidates))
	for _, candidate := range candidates {
		weight := candidateWeight(candidate)
		if weight > 0 {
			positive = append(positive, candidate)
			weights[candidate.ID] = weight
		}
	}
	if len(positive) == 0 {
		return ""
	}
	if len(positive) == 1 {
		return positive[0].ID
	}
	sort.Slice(positive, func(i, j int) bool { return positive[i].ID < positive[j].ID })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[string]map[string]*subsetScheduleState)
	}
	pools := s.keys[scope]
	if pools == nil {
		pools = make(map[string]*subsetScheduleState)
		s.keys[scope] = pools
	}
	state := pools[pool]
	if state == nil {
		if len(pools) >= maxPoolsPerKey {
			pools = make(map[string]*subsetScheduleState)
			s.keys[scope] = pools
		}
		state = &subsetScheduleState{Current: make(map[string]int64), Weights: make(map[string]int64)}
		pools[pool] = state
	}
	weightsChanged := false
	for _, candidate := range positive {
		weight := weights[candidate.ID]
		if old, ok := state.Weights[candidate.ID]; ok && old != weight {
			weightsChanged = true
		}
		state.Weights[candidate.ID] = weight
	}
	if weightsChanged {
		state.Current = make(map[string]int64)
	}
	if len(state.Current) > maxCredentialsPerPool {
		current := make(map[string]int64, len(positive))
		keptWeights := make(map[string]int64, len(positive))
		for _, c := range positive {
			current[c.ID] = state.Current[c.ID]
			keptWeights[c.ID] = state.Weights[c.ID]
		}
		state.Current = current
		state.Weights = keptWeights
	}
	var selected string
	var highest int64
	total := int64(0)
	for _, candidate := range positive {
		weight := state.Weights[candidate.ID]
		total += weight
		state.Current[candidate.ID] += weight
		score := state.Current[candidate.ID]
		if selected == "" || score > highest || (score == highest && candidate.ID < selected) {
			selected = candidate.ID
			highest = score
		}
	}
	state.Current[selected] -= total
	return selected
}

func (s *subsetScheduler) prune(scopes map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for scope := range s.keys {
		if _, ok := scopes[scope]; !ok {
			delete(s.keys, scope)
		}
	}
}

func candidateAllowed(candidate SchedulerAuthCandidate, decision billing.RoutingDecision) bool {
	// Permissions from every enabled group are combined before this check. A
	// grant never re-enables an upstream account that CPA marks as disabled.
	// Do not reject status=error here: the host applies model-specific cooldowns
	// before supplying candidates, so another model may still use that account.
	return strings.TrimSpace(candidate.ID) != "" &&
		!strings.EqualFold(strings.TrimSpace(candidate.Status), "disabled") &&
		candidateWeight(candidate) > 0 &&
		routingAllowsCredential(candidate.ID, credentialSourceFromCandidate(candidate), candidate.Provider, decision)
}

// breakoutCandidates is the opt-in State retry pool. CPA drops accounts a
// request already tried, so once the key's groups have none left, a selected
// account holding a valid template for this model may serve the retry.
func (a *App) breakoutCandidates(req SchedulerPickRequest) []SchedulerAuthCandidate {
	if isTurnStateImageRequest(req.Options.Metadata) || !a.turnState.Active() {
		return nil
	}
	var ready []SchedulerAuthCandidate
	for _, candidate := range req.Candidates {
		if strings.TrimSpace(candidate.ID) != "" && !strings.EqualFold(strings.TrimSpace(candidate.Status), "disabled") &&
			candidateWeight(candidate) > 0 && a.turnState.BreakoutReady(candidate.ID, req.Model) {
			ready = append(ready, candidate)
		}
	}
	return ready
}

func (a *App) pickCredential(raw []byte) ([]byte, error) {
	var req SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("Parse upstream credential scheduling parameters: %w", err)
	}
	if a == nil || a.store == nil || !a.store.Enabled() {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	if metadataString(req.Options.Metadata, MetadataSource) == SourcePluginHostModelCallback {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	a.observeCandidates(req.Candidates)
	scope := metadataString(req.Options.Metadata, MetadataCallerScope)
	requestedModel := metadataString(req.Options.Metadata, MetadataRequestedModel)
	if requestedModel == "" {
		requestedModel = req.Model
	}
	decision := a.store.ResolveRouting(scope, req.Model, requestedModel)
	if decision.AccessDenied != "" {
		return ErrorEnvelope("access_denied", decision.AccessDenied, http.StatusForbidden), nil
	}
	if decision.ConfigurationError != "" {
		return ErrorEnvelope("routing_configuration_error", decision.ConfigurationError, http.StatusServiceUnavailable), nil
	}
	protectAccounts := !isTurnStateImageRequest(req.Options.Metadata) && a.turnStateGate() && a.turnState.HasProtectedAccounts()
	if !decision.RestrictsCredentials() && !protectAccounts {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	allowed := make([]SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidateAllowed(candidate, decision) && (!protectAccounts || !a.turnState.AccountProtected(candidate.ID) || a.hostSchema.Load() >= 6 && a.turnState.BusinessReady(candidate.ID, req.Model)) {
			allowed = append(allowed, candidate)
		}
	}
	if len(allowed) == 0 {
		allowed = a.breakoutCandidates(req)
	}
	if len(allowed) == 0 {
		return ErrorEnvelope("no_routed_credential", noRoutedCredentialMessage, http.StatusServiceUnavailable), nil
	}
	if len(allowed) == len(req.Candidates) {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	id := a.scheduler.pick(scope, routingPoolKey(decision.Model, decision), allowed)
	if id == "" {
		return ErrorEnvelope("no_routed_credential", noRoutedCredentialMessage, http.StatusServiceUnavailable), nil
	}
	return OKEnvelope(SchedulerPickResponse{AuthID: id, Handled: true})
}
