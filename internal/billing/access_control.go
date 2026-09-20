package billing

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

type AccessControl struct {
	Enabled       bool `json:"enabled"`
	DenyUngrouped bool `json:"deny_ungrouped"`
}

// A group grants what its bound routes and its own Rule select. Rule is merged
// exactly like a bound route's rule, so operators can select credentials and
// models directly without creating a route first.
type KeyGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Disabled    bool      `json:"disabled"`
	RoutingMode string    `json:"routing_mode"`
	RouteIDs    []string  `json:"route_ids"`
	Rule        RouteRule `json:"rule"`
}

type GroupView struct {
	KeyGroup
	Scopes []string `json:"scopes"`
}

type GroupPatch struct {
	ID          string     `json:"id"`
	Name        *string    `json:"name,omitempty"`
	Disabled    *bool      `json:"disabled,omitempty"`
	RoutingMode *string    `json:"routing_mode,omitempty"`
	RouteIDs    *[]string  `json:"route_ids,omitempty"`
	Rule        *RouteRule `json:"rule,omitempty"`
	Scopes      *[]string  `json:"scopes,omitempty"`
}

const (
	GroupRoutingOrdinary  = "ordinary"
	GroupRoutingExclusive = "exclusive"
	GroupRoutingCommon    = "common"
)

func normalizeGroupRoutingMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return GroupRoutingOrdinary, nil
	}
	switch mode {
	case GroupRoutingOrdinary, GroupRoutingExclusive, GroupRoutingCommon:
		return mode, nil
	default:
		return "", invalidf("Group routing mode must be ordinary, exclusive, or common")
	}
}

func (s *Store) AccessControl() AccessControl {
	var settings AccessControl
	s.read(func(state *State) { settings = state.AccessControl })
	return settings
}

func (s *Store) SetAccessControl(settings AccessControl) error {
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		state.AccessControl = settings
		return struct{}{}, Changes{AccessControl: true}, nil
	})
	return err
}

func cloneGroup(group KeyGroup) KeyGroup {
	if group.RoutingMode == "" {
		group.RoutingMode = GroupRoutingOrdinary
	}
	group.RouteIDs = append([]string{}, group.RouteIDs...)
	group.Rule = group.Rule.clone()
	return group
}

func NormalizeGroup(group KeyGroup) (KeyGroup, error) {
	group.ID, group.Name = strings.TrimSpace(group.ID), strings.TrimSpace(group.Name)
	if group.ID == "" || len(group.ID) > maxRouteValueBytes {
		return KeyGroup{}, invalidf("Invalid group ID")
	}
	if group.Name == "" || len(group.Name) > maxRouteNameBytes {
		return KeyGroup{}, invalidf("Group name is required and must not exceed %d bytes", maxRouteNameBytes)
	}
	var err error
	if group.RoutingMode, err = normalizeGroupRoutingMode(group.RoutingMode); err != nil {
		return KeyGroup{}, err
	}
	if group.RouteIDs, err = normalizeRouteStrings(group.RouteIDs); err != nil {
		return KeyGroup{}, err
	}
	if group.Rule, err = NormalizeRouteRule(group.Rule); err != nil {
		return KeyGroup{}, err
	}
	return group, nil
}

// grantsAccess reports whether the group configures any permission: a bound
// route or a direct selection. An unconfigured group denies its members.
func (g KeyGroup) grantsAccess() bool {
	return len(g.RouteIDs) > 0 || !g.Rule.empty()
}

func (s *Store) Group(id string) (KeyGroup, bool) {
	var result KeyGroup
	found := false
	s.read(func(state *State) {
		if i := state.findGroupIndex(strings.TrimSpace(id)); i >= 0 {
			result, found = cloneGroup(state.Groups[i]), true
		}
	})
	return result, found
}

func (s *State) findGroupIndex(id string) int {
	return slices.IndexFunc(s.Groups, func(group KeyGroup) bool { return group.ID == id })
}

func (s *State) groupView(group KeyGroup) GroupView {
	view := GroupView{KeyGroup: cloneGroup(group), Scopes: []string{}}
	for scope, key := range s.Keys {
		if key != nil && slices.Contains(key.GroupIDs, group.ID) {
			view.Scopes = append(view.Scopes, scope)
		}
	}
	sort.Strings(view.Scopes)
	return view
}

func (s *Store) GroupViews() []GroupView {
	views := []GroupView{}
	s.read(func(state *State) {
		for _, group := range state.Groups {
			views = append(views, state.groupView(group))
		}
	})
	return views
}

func validateGroupRoutes(state *State, group KeyGroup) error {
	for _, id := range group.RouteIDs {
		if _, exists := state.findRoute(id); !exists {
			return notFoundf("Routing rule %q does not exist", id)
		}
	}
	return nil
}

// Only explicitly bound, enabled groups participate. One exclusive group
// replaces ordinary group contributions; common groups remain additive.
// Direct API-key restrictions are applied separately by routing resolution.
func (s *State) effectiveKeyGroups(ids []string) ([]KeyGroup, error) {
	groups := make([]KeyGroup, 0, len(ids))
	var exclusiveID string
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		i := s.findGroupIndex(id)
		if i < 0 {
			return nil, fmt.Errorf("Group %q no longer exists", id)
		}
		group := s.Groups[i]
		if group.Disabled {
			continue
		}
		mode, err := normalizeGroupRoutingMode(group.RoutingMode)
		if err != nil {
			return nil, err
		}
		group.RoutingMode = mode
		if mode == GroupRoutingExclusive {
			if exclusiveID != "" {
				return nil, conflictf("An API key can belong to only one enabled exclusive group; groups %q and %q conflict", exclusiveID, id)
			}
			exclusiveID = id
		}
		groups = append(groups, group)
	}
	if exclusiveID != "" {
		groups = slices.DeleteFunc(groups, func(group KeyGroup) bool { return group.RoutingMode == GroupRoutingOrdinary })
	}
	return groups, nil
}

func validateExclusiveGroupBindings(state *State, scopes []string) error {
	for _, scope := range scopes {
		if key := state.Keys[scope]; key != nil {
			if _, err := state.effectiveKeyGroups(key.GroupIDs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) CreateGroup(group KeyGroup, scopes []string) (GroupView, error) {
	scopes = normalizeScopes(scopes)
	return editConfiguration(s, func(state *State) (GroupView, Changes, error) {
		if strings.TrimSpace(group.ID) == "" {
			group.ID = freeID(group.Name, "group", func(id string) bool { return state.findGroupIndex(id) >= 0 })
		}
		validated, err := NormalizeGroup(group)
		if err != nil {
			return GroupView{}, Changes{}, err
		}
		group = validated
		if state.findGroupIndex(group.ID) >= 0 {
			return GroupView{}, Changes{}, conflictf("Group %q already exists", group.ID)
		}
		if err := validateGroupRoutes(state, group); err != nil {
			return GroupView{}, Changes{}, err
		}
		for _, scope := range scopes {
			if state.liveKey(scope) == nil {
				return GroupView{}, Changes{}, notFoundf("API key %q does not exist", scope)
			}
		}
		state.Groups = append(state.Groups, group)
		for _, scope := range scopes {
			state.Keys[scope].GroupIDs = append(state.Keys[scope].GroupIDs, group.ID)
		}
		if err := validateExclusiveGroupBindings(state, scopes); err != nil {
			return GroupView{}, Changes{}, err
		}
		return state.groupView(group), Changes{Groups: true, Keys: scopes}, nil
	})
}

func (s *Store) UpdateGroup(patch GroupPatch) (GroupView, error) {
	patch.ID = strings.TrimSpace(patch.ID)
	return editConfiguration(s, func(state *State) (GroupView, Changes, error) {
		i := state.findGroupIndex(patch.ID)
		if i < 0 {
			return GroupView{}, Changes{}, notFoundf("Group %q does not exist", patch.ID)
		}
		group := state.Groups[i]
		if patch.Name != nil {
			group.Name = *patch.Name
		}
		if patch.Disabled != nil {
			group.Disabled = *patch.Disabled
		}
		if patch.RoutingMode != nil {
			group.RoutingMode = *patch.RoutingMode
		}
		if patch.RouteIDs != nil {
			group.RouteIDs = *patch.RouteIDs
		}
		if patch.Rule != nil {
			group.Rule = *patch.Rule
		}
		group, err := NormalizeGroup(group)
		if err != nil {
			return GroupView{}, Changes{}, err
		}
		if err := validateGroupRoutes(state, group); err != nil {
			return GroupView{}, Changes{}, err
		}
		var changed []string
		if patch.Scopes != nil {
			selected := normalizeScopes(*patch.Scopes)
			for _, scope := range selected {
				key := state.Keys[scope]
				if key == nil || !key.DeletedAt.IsZero() && !slices.Contains(key.GroupIDs, group.ID) {
					return GroupView{}, Changes{}, notFoundf("API key %q does not exist", scope)
				}
			}
			for scope, key := range state.Keys {
				if key == nil {
					continue
				}
				has, want := slices.Contains(key.GroupIDs, group.ID), slices.Contains(selected, scope)
				if has == want {
					continue
				}
				if want {
					key.GroupIDs = append(key.GroupIDs, group.ID)
				} else {
					key.GroupIDs = slices.DeleteFunc(key.GroupIDs, func(id string) bool { return id == group.ID })
				}
				changed = append(changed, scope)
			}
		}
		state.Groups[i] = group
		if err := validateExclusiveGroupBindings(state, state.groupView(group).Scopes); err != nil {
			return GroupView{}, Changes{}, err
		}
		return state.groupView(group), Changes{Groups: true, Keys: changed}, nil
	})
}

func (s *Store) DeleteGroup(id string) error {
	id = strings.TrimSpace(id)
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		i := state.findGroupIndex(id)
		if i < 0 {
			return struct{}{}, Changes{}, notFoundf("Group %q does not exist", id)
		}
		var changed []string
		for scope, key := range state.Keys {
			if key != nil && slices.Contains(key.GroupIDs, id) {
				key.GroupIDs = slices.DeleteFunc(key.GroupIDs, func(value string) bool { return value == id })
				changed = append(changed, scope)
			}
		}
		state.Groups = slices.Delete(state.Groups, i, i+1)
		return struct{}{}, Changes{Groups: true, Keys: changed}, nil
	})
	return err
}

func (s *Store) SetKeyGroups(scopes, groupIDs []string) error {
	scopes = normalizeScopes(scopes)
	if len(scopes) == 0 {
		return invalidf("Select at least one API key")
	}
	groupIDs, err := normalizeRouteStrings(groupIDs)
	if err != nil {
		return err
	}
	_, err = editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		for _, id := range groupIDs {
			if state.findGroupIndex(id) < 0 {
				return struct{}{}, Changes{}, notFoundf("Group %q does not exist", id)
			}
		}
		for _, scope := range scopes {
			if state.liveKey(scope) == nil {
				return struct{}{}, Changes{}, notFoundf("API key %q does not exist", scope)
			}
		}
		for _, scope := range scopes {
			state.Keys[scope].GroupIDs = append([]string(nil), groupIDs...)
		}
		if err := validateExclusiveGroupBindings(state, scopes); err != nil {
			return struct{}{}, Changes{}, err
		}
		return struct{}{}, Changes{Keys: scopes}, nil
	})
	return err
}
