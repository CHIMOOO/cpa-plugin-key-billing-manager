package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

const (
	CredentialSourceAuthFiles   = "auth-files"
	CredentialSourceAIProviders = "ai-providers"

	maxRouteNameBytes  = 128
	maxRouteValueBytes = 512
)

type CredentialProviderSelector struct {
	Source   string `json:"source"`
	Provider string `json:"provider"`
}

type RouteRule struct {
	Models                    []string                     `json:"models"`
	CredentialIDs             []string                     `json:"credential_ids"`
	CredentialProviders       []CredentialProviderSelector `json:"credential_providers"`
	DeniedModels              []string                     `json:"denied_models"`
	DeniedCredentialIDs       []string                     `json:"denied_credential_ids"`
	DeniedCredentialProviders []CredentialProviderSelector `json:"denied_credential_providers"`
}

type RouteBindings struct {
	RouteIDs []string `json:"route_ids"`
	// Configured distinguishes an explicitly emptied rule from an untouched key.
	Configured bool `json:"configured,omitempty"`
	RouteRule
}

type Route struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Rule RouteRule `json:"rule"`
}

type RoutePatch struct {
	ID   string     `json:"id"`
	Name *string    `json:"name,omitempty"`
	Rule *RouteRule `json:"rule,omitempty"`
}

type RoutingDecision struct {
	RouteRule
	Model                      string
	ConfigurationError         string
	AccessDenied               string
	RequireCredentialAllowlist bool
	// An exclusive group defines the maximum pool. A key's explicit direct
	// allowlist may narrow that pool, including dynamic provider selectors.
	CredentialConstraint *RouteRule
	// A group that grants credentials without listing models allows every
	// model on those credentials, even when another group lists models. Such
	// credentials alone serve models outside pairedModels.
	openCredentials *RouteRule
	pairedModels    []string
}

// Called once the requested model is known: a model that only an unlisted
// grant covers may use only that grant's credentials.
func (d *RoutingDecision) narrowToModel() {
	if d.openCredentials == nil || d.Model == "" || containsRouteValue(d.pairedModels, d.Model) {
		return
	}
	d.CredentialIDs = slices.Clone(d.openCredentials.CredentialIDs)
	d.CredentialProviders = slices.Clone(d.openCredentials.CredentialProviders)
}

func (d RoutingDecision) RestrictsModels() bool {
	return d.AccessDenied != "" || len(d.Models) > 0 || len(d.DeniedModels) > 0
}

func (d RoutingDecision) AllowsModel() bool {
	if d.ConfigurationError != "" || d.AccessDenied != "" {
		return false
	}
	if d.Model == "" {
		return true
	}
	return !containsRouteValue(d.DeniedModels, d.Model) &&
		(len(d.Models) == 0 || containsRouteValue(d.Models, d.Model))
}

func containsRouteValue(values []string, value string) bool {
	return slices.ContainsFunc(values, func(item string) bool { return strings.EqualFold(item, value) })
}

func (d RoutingDecision) RestrictsCredentials() bool {
	return d.CredentialConstraint != nil || d.RequireCredentialAllowlist || d.AccessDenied != "" || len(d.CredentialIDs) > 0 || len(d.CredentialProviders) > 0 ||
		len(d.DeniedCredentialIDs) > 0 || len(d.DeniedCredentialProviders) > 0
}

// ref is a fingerprint, never the raw host credential ID or an API key.
func (d RoutingDecision) AllowsCredential(ref, source, provider string) bool {
	if d.CredentialConstraint != nil && !(RoutingDecision{RouteRule: *d.CredentialConstraint, RequireCredentialAllowlist: true}).AllowsCredential(ref, source, provider) {
		return false
	}
	selector := CredentialProviderSelector{Source: strings.ToLower(strings.TrimSpace(source)), Provider: strings.ToLower(strings.TrimSpace(provider))}
	if d.ConfigurationError != "" || d.AccessDenied != "" || containsRouteValue(d.DeniedCredentialIDs, ref) {
		return false
	}
	for _, denied := range d.DeniedCredentialProviders {
		// Incomplete host metadata cannot prove a candidate is outside a deny
		// selector. Reject potential matches without inventing its source.
		if (selector.Source == "" || selector.Source == denied.Source) &&
			(selector.Provider == "" || selector.Provider == denied.Provider) {
			return false
		}
	}
	return !d.RequireCredentialAllowlist && len(d.CredentialIDs) == 0 && len(d.CredentialProviders) == 0 ||
		containsRouteValue(d.CredentialIDs, ref) || slices.Contains(d.CredentialProviders, selector)
}

func (r RouteRule) empty() bool {
	return len(r.Models) == 0 && len(r.CredentialIDs) == 0 && len(r.CredentialProviders) == 0 &&
		len(r.DeniedModels) == 0 && len(r.DeniedCredentialIDs) == 0 && len(r.DeniedCredentialProviders) == 0
}

func (r RouteRule) CredentialRefs() []string {
	return append(slices.Clone(r.CredentialIDs), r.DeniedCredentialIDs...)
}

func (r RouteRule) clone() RouteRule {
	return RouteRule{
		Models:                    append([]string{}, r.Models...),
		CredentialIDs:             append([]string{}, r.CredentialIDs...),
		CredentialProviders:       append([]CredentialProviderSelector{}, r.CredentialProviders...),
		DeniedModels:              append([]string{}, r.DeniedModels...),
		DeniedCredentialIDs:       append([]string{}, r.DeniedCredentialIDs...),
		DeniedCredentialProviders: append([]CredentialProviderSelector{}, r.DeniedCredentialProviders...),
	}
}

func (b RouteBindings) clone() RouteBindings {
	return RouteBindings{RouteIDs: append([]string{}, b.RouteIDs...), Configured: b.Configured, RouteRule: b.RouteRule.clone()}
}

type RouteDeleteResult struct {
	Deleted               string `json:"deleted"`
	AffectedKeys          int    `json:"affected_keys"`
	DeletedKeys           int    `json:"deleted_keys"`
	FullyUnrestrictedKeys int    `json:"fully_unrestricted_keys"`
}

type RouteView struct {
	Route
	BoundKeyCount         int `json:"bound_key_count"`
	DeletedKeyCount       int `json:"deleted_key_count"`
	FullyUnrestrictedKeys int `json:"fully_unrestricted_keys"`
}

func CredentialFingerprint(rawID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("cpa-key-billing:credential:v1\x00"))
	_, _ = h.Write([]byte(rawID))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (s *Store) ConfigCredentials() map[string]ConfigCredential {
	var credentials map[string]ConfigCredential
	s.read(func(state *State) { credentials = maps.Clone(state.ConfigCredentials) })
	return credentials
}

func (s *Store) SyncConfigCredentials(credentials map[string]ConfigCredential) error {
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		if maps.Equal(state.ConfigCredentials, credentials) {
			return struct{}{}, Changes{}, nil
		}
		state.ConfigCredentials = maps.Clone(credentials)
		return struct{}{}, Changes{ConfigCredentials: true}, nil
	})
	return err
}

func ValidCredentialFingerprint(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(value[len("sha256:"):])
	return err == nil
}

func normalizeCredentialProviderSelector(item CredentialProviderSelector) (CredentialProviderSelector, error) {
	item.Source = strings.ToLower(strings.TrimSpace(item.Source))
	item.Provider = strings.ToLower(strings.TrimSpace(item.Provider))
	if item.Source != CredentialSourceAuthFiles && item.Source != CredentialSourceAIProviders {
		return CredentialProviderSelector{}, invalidf("Upstream credential source must be auth-files or ai-providers")
	}
	if item.Provider == "" || len(item.Provider) > maxRouteValueBytes || strings.ContainsAny(item.Provider, "*[]\x00") {
		return CredentialProviderSelector{}, invalidf("Invalid provider identifier")
	}
	return item, nil
}

func NormalizeRouteRule(rule RouteRule) (RouteRule, error) {
	var err error
	rule.Models, rule.DeniedModels, err = normalizeRouteSelection(
		rule.Models, rule.DeniedModels, normalizeRouteStrings, strings.EqualFold, "model",
	)
	if err != nil {
		return RouteRule{}, err
	}
	rule.CredentialIDs, rule.DeniedCredentialIDs, err = normalizeRouteSelection(
		rule.CredentialIDs, rule.DeniedCredentialIDs, normalizeCredentialIDs, strings.EqualFold, "credential",
	)
	if err != nil {
		return RouteRule{}, err
	}
	rule.CredentialProviders, rule.DeniedCredentialProviders, err = normalizeRouteSelection(
		rule.CredentialProviders, rule.DeniedCredentialProviders, normalizeCredentialProviders,
		func(a, b CredentialProviderSelector) bool { return a == b }, "credential category",
	)
	if err != nil {
		return RouteRule{}, err
	}
	return rule, nil
}

// Within one rule each item has exactly one state. Different bound rules may
// disagree; resolution preserves both selections so that deny takes precedence.
func normalizeRouteSelection[T any](allow, deny []T, normalize func([]T) ([]T, error), equal func(T, T) bool, name string) ([]T, []T, error) {
	allow, err := normalize(allow)
	if err != nil {
		return nil, nil, err
	}
	deny, err = normalize(deny)
	if err != nil {
		return nil, nil, err
	}
	for _, value := range deny {
		if slices.ContainsFunc(allow, func(item T) bool { return equal(item, value) }) {
			switch name {
			case "model":
				return nil, nil, invalidf("A model cannot be both allowed and denied")
			case "credential":
				return nil, nil, invalidf("A credential cannot be both allowed and denied")
			default:
				return nil, nil, invalidf("A credential category cannot be both allowed and denied")
			}
		}
	}
	return allow, deny, nil
}

func normalizeCredentialProviders(values []CredentialProviderSelector) ([]CredentialProviderSelector, error) {
	result := make([]CredentialProviderSelector, 0, len(values))
	seen := make(map[CredentialProviderSelector]struct{}, len(values))
	for _, item := range values {
		item, err := normalizeCredentialProviderSelector(item)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}

func normalizeRouteStrings(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxRouteValueBytes {
			return nil, invalidf("Invalid routing option")
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeCredentialIDs(values []string) ([]string, error) {
	values, err := normalizeRouteStrings(values)
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		if !ValidCredentialFingerprint(value) {
			return nil, invalidf("Invalid upstream credential reference")
		}
		values[i] = strings.ToLower(value)
	}
	return values, nil
}

func NormalizeRouteBindings(bindings RouteBindings) (RouteBindings, error) {
	var err error
	bindings.RouteIDs, err = normalizeRouteStrings(bindings.RouteIDs)
	if err != nil {
		return RouteBindings{}, err
	}
	bindings.RouteRule, err = NormalizeRouteRule(bindings.RouteRule)
	if err != nil {
		return RouteBindings{}, err
	}

	return bindings, nil
}

func (s *State) findRouteIndex(id string) int {
	id = strings.TrimSpace(id)
	return slices.IndexFunc(s.Routes, func(route Route) bool { return route.ID == id })
}

func (s *State) findRoute(id string) (Route, bool) {
	i := s.findRouteIndex(id)
	if i < 0 {
		return Route{}, false
	}
	return s.Routes[i], true
}

func cloneRoute(route Route) Route {
	route.Rule = route.Rule.clone()
	return route
}

func (s *Store) Route(id string) (Route, bool) {
	var result Route
	found := false
	s.read(func(state *State) {
		if route, ok := state.findRoute(id); ok {
			result, found = cloneRoute(route), true
		}
	})
	return result, found
}

func (s *Store) RouteViews() []RouteView {
	views := []RouteView{}
	s.read(func(state *State) {
		for _, route := range state.Routes {
			view := RouteView{Route: cloneRoute(route)}
			for _, key := range state.Keys {
				if key == nil {
					continue
				}
				groupBound := state.groupBindsRoute(key, route.ID)
				if !groupBound && !slices.Contains(key.RouteBindings.RouteIDs, route.ID) {
					continue
				}
				view.BoundKeyCount++
				if !key.DeletedAt.IsZero() {
					view.DeletedKeyCount++
					continue
				}
				if groupBound {
					// Deleting a route still used by a group is prohibited.
					continue
				}
				copyKey := *key
				copyKey.RouteBindings.RouteIDs = slices.DeleteFunc(slices.Clone(key.RouteBindings.RouteIDs), func(id string) bool { return id == route.ID })
				if !routingRestricted(state, &copyKey) {
					view.FullyUnrestrictedKeys++
				}
			}
			views = append(views, view)
		}
	})
	return views
}

func (s *State) groupBindsRoute(key *KeyState, routeID string) bool {
	for _, id := range key.GroupIDs {
		if i := s.findGroupIndex(id); i >= 0 && slices.Contains(s.Groups[i].RouteIDs, routeID) {
			return true
		}
	}
	return false
}

func NormalizeRoute(route Route) (Route, error) {
	route.ID = strings.TrimSpace(route.ID)
	if route.ID == "" {
		return Route{}, invalidf("Routing rule ID is required")
	}
	route.Name = strings.TrimSpace(route.Name)
	if route.Name == "" {
		return Route{}, invalidf("Routing rule name is required")
	}
	if len([]byte(route.Name)) > maxRouteNameBytes {
		return Route{}, invalidf("Routing rule name must not exceed %d bytes", maxRouteNameBytes)
	}
	rule, err := NormalizeRouteRule(route.Rule)
	if err != nil {
		return Route{}, err
	}
	route.Rule = rule
	return route, nil
}

func (s *Store) CreateRoute(route Route, scopes []string) (Route, error) {
	scopes = normalizeScopes(scopes)
	return editConfiguration(s, func(state *State) (Route, Changes, error) {
		for _, scope := range scopes {
			if state.liveKey(scope) == nil {
				return Route{}, Changes{}, notFoundf("API key %q does not exist", scope)
			}
		}
		if strings.TrimSpace(route.ID) == "" {
			route.ID = freeID(route.Name, "route", func(id string) bool {
				_, exists := state.findRoute(id)
				return exists
			})
		}
		validated, err := NormalizeRoute(route)
		if err != nil {
			return Route{}, Changes{}, err
		}
		route = validated
		if _, ok := state.findRoute(route.ID); ok {
			return Route{}, Changes{}, conflictf("Routing rule %q already exists", route.ID)
		}
		state.Routes = append(state.Routes, route)
		for _, scope := range scopes {
			state.Keys[scope].RouteBindings.RouteIDs = append(state.Keys[scope].RouteBindings.RouteIDs, route.ID)
		}
		return cloneRoute(route), Changes{Routes: true, Keys: scopes}, nil
	})
}

func (s *Store) UpdateRoute(patch RoutePatch, scopes *[]string) (Route, error) {
	patch.ID = strings.TrimSpace(patch.ID)
	if patch.ID == "" {
		return Route{}, invalidf("Routing rule ID is required")
	}
	return editConfiguration(s, func(state *State) (Route, Changes, error) {
		var changed []string
		i := state.findRouteIndex(patch.ID)
		if i < 0 {
			return Route{}, Changes{}, notFoundf("Routing rule %q does not exist", patch.ID)
		}
		updated := state.Routes[i]
		if patch.Name != nil {
			updated.Name = *patch.Name
		}
		if patch.Rule != nil {
			updated.Rule = *patch.Rule
		}
		validated, err := NormalizeRoute(updated)
		if err != nil {
			return Route{}, Changes{}, err
		}
		var selected map[string]struct{}
		if scopes != nil {
			normalized := normalizeScopes(*scopes)
			selected = make(map[string]struct{}, len(normalized))
			for _, scope := range normalized {
				key := state.Keys[scope]
				if key == nil || !key.DeletedAt.IsZero() && !slices.Contains(key.RouteBindings.RouteIDs, patch.ID) {
					return Route{}, Changes{}, notFoundf("API key %q does not exist", scope)
				}
				selected[scope] = struct{}{}
			}
		}
		updated = validated
		state.Routes[i] = updated
		if scopes != nil {
			for scope, key := range state.Keys {
				if key == nil {
					continue
				}
				hasBinding := slices.Contains(key.RouteBindings.RouteIDs, patch.ID)
				_, shouldBind := selected[scope]
				if shouldBind && !hasBinding {
					key.RouteBindings.RouteIDs = append(key.RouteBindings.RouteIDs, patch.ID)
					changed = append(changed, scope)
				} else if !shouldBind && hasBinding {
					key.RouteBindings.RouteIDs = slices.DeleteFunc(key.RouteBindings.RouteIDs, func(id string) bool { return id == patch.ID })
					changed = append(changed, scope)
				}
			}
		}
		return cloneRoute(updated), Changes{Routes: true, Keys: changed}, nil
	})
}

func (s *Store) SetKeyRoutes(scope string, bindings RouteBindings) error {
	scope = normalizeScope(scope)
	if scope == "" {
		return invalidf("API key identifier is required")
	}
	bindings, err := NormalizeRouteBindings(bindings)
	if err != nil {
		return err
	}
	bindings.Configured = true
	_, err = editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		key := state.liveKey(scope)
		if key == nil {
			return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
		}
		for _, id := range bindings.RouteIDs {
			if _, ok := state.findRoute(id); !ok {
				return struct{}{}, Changes{}, notFoundf("Routing rule %q does not exist", id)
			}
		}
		key.RouteBindings = bindings
		return struct{}{}, Changes{Keys: []string{scope}}, nil
	})
	return err
}

func (s *Store) DeleteRoute(id string) (RouteDeleteResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return RouteDeleteResult{}, invalidf("Routing rule ID is required")
	}
	return editConfiguration(s, func(state *State) (RouteDeleteResult, Changes, error) {
		var changed []string
		i := state.findRouteIndex(id)
		if i < 0 {
			return RouteDeleteResult{}, Changes{}, notFoundf("Routing rule %q does not exist", id)
		}
		for _, group := range state.Groups {
			if slices.Contains(group.RouteIDs, id) {
				return RouteDeleteResult{}, Changes{}, conflictf("Routing rule is still used by group %q; remove its group binding first", group.Name)
			}
		}
		out := RouteDeleteResult{Deleted: id}
		for scope, key := range state.Keys {
			if key == nil {
				continue
			}
			before := len(key.RouteBindings.RouteIDs)
			key.RouteBindings.RouteIDs = slices.DeleteFunc(key.RouteBindings.RouteIDs, func(routeID string) bool { return routeID == id })
			if len(key.RouteBindings.RouteIDs) == before {
				continue
			}
			changed = append(changed, scope)
			out.AffectedKeys++
			if !key.DeletedAt.IsZero() {
				out.DeletedKeys++
				continue
			}
			if !routingRestricted(state, key) {
				out.FullyUnrestrictedKeys++
			}
		}
		state.Routes = slices.Delete(state.Routes, i, i+1)
		return out, Changes{Routes: true, Keys: changed}, nil
	})
}

func routingRestricted(state *State, key *KeyState) bool {
	decision := resolveRoutingState(state, key)
	return decision.ConfigurationError != "" || decision.AccessDenied != "" || decision.RestrictsModels() || decision.RestrictsCredentials()
}

func (s *Store) ResolveRouting(scope, upstreamModel, routeModel string) RoutingDecision {
	var decision RoutingDecision
	s.read(func(state *State) {
		decision = resolveRoutingState(state, state.Keys[normalizeScope(scope)])
		decision.Model = strings.TrimSpace(state.ResolveBillingModel(upstreamModel, routeModel))
		decision.narrowToModel()
	})
	return decision
}

func (s *Store) KeyDescription(scope string) string {
	result := ""
	s.read(func(state *State) {
		result = keyDescription(state.Keys[normalizeScope(scope)])
	})
	return result
}

// Merge each allow/deny dimension of the bound routes, the groups' direct
// selections and the key's own selection independently, retaining deny entries
// for precedence. Managed keys require an explicit credential allowlist; an empty
// model allowlist still leaves models unrestricted.
func resolveRoutingState(state *State, key *KeyState) RoutingDecision {
	d := RoutingDecision{RouteRule: RouteRule{}.clone()}
	if !state.AccessControl.Enabled {
		return d
	}
	if state.AccessControl.DenyUngrouped && (key == nil || len(key.GroupIDs) == 0) {
		d.AccessDenied = "API key is not assigned to a group; access is denied"
		return d
	}
	if key == nil {
		return d
	}
	routeIDs := append([]string(nil), key.RouteBindings.RouteIDs...)
	groupRules := make([]RouteRule, 0, len(key.GroupIDs))
	groupConfigured := false
	groups, err := state.effectiveKeyGroups(key.GroupIDs)
	if err != nil {
		d.ConfigurationError = err.Error()
		return d
	}
	exclusive := false
	for _, group := range groups {
		exclusive = exclusive || group.RoutingMode == GroupRoutingExclusive
		groupConfigured = groupConfigured || group.grantsAccess()
		routeIDs = append(routeIDs, group.RouteIDs...)
		groupRules = append(groupRules, group.Rule)
	}
	if exclusive {
		// Key-bound routes are constraints in exclusive mode, never another
		// source of grants. Group route IDs are rebuilt without those bindings.
		routeIDs = nil
		for _, group := range groups {
			routeIDs = append(routeIDs, group.RouteIDs...)
		}
	}
	if len(key.GroupIDs) > 0 && !groupConfigured {
		d.AccessDenied = "API key groups have no routing rules or upstream credentials configured; access is denied"
		if len(groups) == 0 {
			d.AccessDenied = "All groups assigned to this API key are disabled; access is denied"
		}
		return d
	}
	direct := RoutingDecision{RouteRule: key.RouteBindings.RouteRule}
	d.RequireCredentialAllowlist = key.RouteBindings.Configured || len(key.GroupIDs) > 0 || len(routeIDs) > 0 || direct.RestrictsModels() || direct.RestrictsCredentials()
	merge := func(rule RouteRule) {
		d.Models = append(d.Models, rule.Models...)
		d.CredentialIDs = append(d.CredentialIDs, rule.CredentialIDs...)
		d.CredentialProviders = append(d.CredentialProviders, rule.CredentialProviders...)
		d.DeniedModels = append(d.DeniedModels, rule.DeniedModels...)
		d.DeniedCredentialIDs = append(d.DeniedCredentialIDs, rule.DeniedCredentialIDs...)
		d.DeniedCredentialProviders = append(d.DeniedCredentialProviders, rule.DeniedCredentialProviders...)
	}
	for _, id := range routeIDs {
		route, ok := state.findRoute(id)
		if !ok {
			d.ConfigurationError = fmt.Sprintf("Routing rule %q no longer exists", id)
			return d
		}
		merge(route.Rule)
	}
	// A group's direct selection counts exactly like one more bound route.
	for _, rule := range groupRules {
		merge(rule)
	}
	units := make([]RouteRule, 0, len(groups)+1)
	for _, group := range groups {
		units = append(units, state.grantUnit(group.RouteIDs, group.Rule))
	}
	if !exclusive {
		units = append(units, state.grantUnit(key.RouteBindings.RouteIDs, key.RouteBindings.RouteRule))
	}
	open := openGrant(units)
	if exclusive {
		constraint := key.RouteBindings.RouteRule.clone()
		for _, id := range key.RouteBindings.RouteIDs {
			route, ok := state.findRoute(id)
			if !ok {
				d.ConfigurationError = fmt.Sprintf("Routing rule %q no longer exists", id)
				return d
			}
			constraint.Models = append(constraint.Models, route.Rule.Models...)
			constraint.CredentialIDs = append(constraint.CredentialIDs, route.Rule.CredentialIDs...)
			constraint.CredentialProviders = append(constraint.CredentialProviders, route.Rule.CredentialProviders...)
			constraint.DeniedModels = append(constraint.DeniedModels, route.Rule.DeniedModels...)
			constraint.DeniedCredentialIDs = append(constraint.DeniedCredentialIDs, route.Rule.DeniedCredentialIDs...)
			constraint.DeniedCredentialProviders = append(constraint.DeniedCredentialProviders, route.Rule.DeniedCredentialProviders...)
		}
		// An untouched key inherits its groups. An explicitly configured direct
		// rule remains a constraint even after its credential allowlist is
		// emptied; model-only and deny-only edits must not restore access.
		if key.RouteBindings.Configured || len(constraint.CredentialIDs) > 0 || len(constraint.CredentialProviders) > 0 {
			d.CredentialConstraint = &constraint
		}
		if len(constraint.Models) > 0 {
			if open != nil {
				// Unlisted grants serve every model the key's own rule allows.
				d.pairedModels = slices.DeleteFunc(slices.Clone(d.Models), func(model string) bool { return !containsRouteValue(constraint.Models, model) })
				d.Models = slices.Clone(constraint.Models)
				d.openCredentials = open
			} else if len(d.Models) == 0 {
				d.Models = slices.Clone(constraint.Models)
			} else {
				d.Models = slices.DeleteFunc(d.Models, func(model string) bool { return !containsRouteValue(constraint.Models, model) })
				if len(d.Models) == 0 {
					d.AccessDenied = "The API key's direct model rules and exclusive group have no models in common; access is denied"
				}
			}
		}
		merge(RouteRule{DeniedModels: constraint.DeniedModels, DeniedCredentialIDs: constraint.DeniedCredentialIDs, DeniedCredentialProviders: constraint.DeniedCredentialProviders})
	} else {
		merge(key.RouteBindings.RouteRule)
	}
	if open != nil && d.openCredentials == nil {
		d.pairedModels, d.Models = d.Models, []string{}
		d.openCredentials = open
	}
	for _, values := range []*[]string{&d.Models, &d.CredentialIDs, &d.DeniedModels, &d.DeniedCredentialIDs} {
		sort.SliceStable(*values, func(i, j int) bool { return strings.ToLower((*values)[i]) < strings.ToLower((*values)[j]) })
		*values = slices.CompactFunc(*values, strings.EqualFold)
	}
	for _, values := range []*[]CredentialProviderSelector{&d.CredentialProviders, &d.DeniedCredentialProviders} {
		sort.Slice(*values, func(i, j int) bool {
			a, b := (*values)[i], (*values)[j]
			if a.Source != b.Source {
				return a.Source < b.Source
			}
			return a.Provider < b.Provider
		})
		*values = slices.Compact(*values)
	}
	return d
}

// grantUnit merges the allow lists of one grant: a group, or a key's own
// bindings, with its routes. Missing routes are reported by the caller.
func (s *State) grantUnit(routeIDs []string, rule RouteRule) RouteRule {
	unit := RouteRule{
		Models:              slices.Clone(rule.Models),
		CredentialIDs:       slices.Clone(rule.CredentialIDs),
		CredentialProviders: slices.Clone(rule.CredentialProviders),
	}
	for _, id := range routeIDs {
		if route, ok := s.findRoute(id); ok {
			unit.Models = append(unit.Models, route.Rule.Models...)
			unit.CredentialIDs = append(unit.CredentialIDs, route.Rule.CredentialIDs...)
			unit.CredentialProviders = append(unit.CredentialProviders, route.Rule.CredentialProviders...)
		}
	}
	return unit
}

// openGrant returns the credentials of grants that list no models, when other
// grants list models. A grant listing models without credentials still limits
// the models of every grant, so it keeps the combined model allowlist.
func openGrant(units []RouteRule) *RouteRule {
	var open *RouteRule
	listed := false
	for _, unit := range units {
		grants := len(unit.CredentialIDs) > 0 || len(unit.CredentialProviders) > 0
		switch {
		case len(unit.Models) > 0 && !grants:
			return nil
		case len(unit.Models) > 0:
			listed = true
		case grants:
			if open == nil {
				open = &RouteRule{CredentialIDs: []string{}, CredentialProviders: []CredentialProviderSelector{}}
			}
			open.CredentialIDs = append(open.CredentialIDs, unit.CredentialIDs...)
			open.CredentialProviders = append(open.CredentialProviders, unit.CredentialProviders...)
		}
	}
	if !listed {
		return nil
	}
	return open
}
