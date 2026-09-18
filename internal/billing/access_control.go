package billing

import (
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
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	RouteIDs []string  `json:"route_ids"`
	Rule     RouteRule `json:"rule"`
}

type GroupView struct {
	KeyGroup
	Scopes []string `json:"scopes"`
}

type GroupPatch struct {
	ID       string     `json:"id"`
	Name     *string    `json:"name,omitempty"`
	RouteIDs *[]string  `json:"route_ids,omitempty"`
	Rule     *RouteRule `json:"rule,omitempty"`
	Scopes   *[]string  `json:"scopes,omitempty"`
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
	group.RouteIDs = append([]string{}, group.RouteIDs...)
	group.Rule = group.Rule.clone()
	return group
}

func NormalizeGroup(group KeyGroup) (KeyGroup, error) {
	group.ID, group.Name = strings.TrimSpace(group.ID), strings.TrimSpace(group.Name)
	if group.ID == "" || len(group.ID) > maxRouteValueBytes {
		return KeyGroup{}, invalidf("分组 ID 无效")
	}
	if group.Name == "" || len(group.Name) > maxRouteNameBytes {
		return KeyGroup{}, invalidf("分组名称不能为空且不能超过 %d 字节", maxRouteNameBytes)
	}
	var err error
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
			return notFoundf("路由规则 %q 不存在", id)
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
			return GroupView{}, Changes{}, conflictf("分组 %q 已存在", group.ID)
		}
		if err := validateGroupRoutes(state, group); err != nil {
			return GroupView{}, Changes{}, err
		}
		for _, scope := range scopes {
			if state.liveKey(scope) == nil {
				return GroupView{}, Changes{}, notFoundf("API Key %q 不存在", scope)
			}
		}
		state.Groups = append(state.Groups, group)
		for _, scope := range scopes {
			state.Keys[scope].GroupIDs = append(state.Keys[scope].GroupIDs, group.ID)
		}
		return state.groupView(group), Changes{Groups: true, Keys: scopes}, nil
	})
}

func (s *Store) UpdateGroup(patch GroupPatch) (GroupView, error) {
	patch.ID = strings.TrimSpace(patch.ID)
	return editConfiguration(s, func(state *State) (GroupView, Changes, error) {
		i := state.findGroupIndex(patch.ID)
		if i < 0 {
			return GroupView{}, Changes{}, notFoundf("分组 %q 不存在", patch.ID)
		}
		group := state.Groups[i]
		if patch.Name != nil {
			group.Name = *patch.Name
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
					return GroupView{}, Changes{}, notFoundf("API Key %q 不存在", scope)
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
		return state.groupView(group), Changes{Groups: true, Keys: changed}, nil
	})
}

func (s *Store) DeleteGroup(id string) error {
	id = strings.TrimSpace(id)
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		i := state.findGroupIndex(id)
		if i < 0 {
			return struct{}{}, Changes{}, notFoundf("分组 %q 不存在", id)
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
		return invalidf("请选择 API Key")
	}
	groupIDs, err := normalizeRouteStrings(groupIDs)
	if err != nil {
		return err
	}
	_, err = editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		for _, id := range groupIDs {
			if state.findGroupIndex(id) < 0 {
				return struct{}{}, Changes{}, notFoundf("分组 %q 不存在", id)
			}
		}
		for _, scope := range scopes {
			if state.liveKey(scope) == nil {
				return struct{}{}, Changes{}, notFoundf("API Key %q 不存在", scope)
			}
		}
		for _, scope := range scopes {
			state.Keys[scope].GroupIDs = append([]string(nil), groupIDs...)
		}
		return struct{}{}, Changes{Keys: scopes}, nil
	})
	return err
}
