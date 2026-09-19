package billing

import (
	"maps"
	"slices"
	"strings"
)

// MigrateCredentialRefs prepares or completes an exact host credential
// replacement. Preparing retains both identities so an interrupted channel
// update cannot remove an existing grant or deny. Completion retires old refs
// only after the management caller has synchronized the new host inventory.
// Groups, shared routes, direct key rules and configured credentials commit in
// one repository transaction; a failed save leaves the working state intact.
func (s *Store) MigrateCredentialRefs(mapping map[string]string, complete bool) error {
	if len(mapping) > 32 {
		return invalidf("Too many credential replacements")
	}
	for old, next := range mapping {
		if !ValidCredentialFingerprint(old) || !ValidCredentialFingerprint(next) || old != strings.ToLower(old) || next != strings.ToLower(next) || old == next {
			return invalidf("Invalid credential replacement")
		}
		if _, chained := mapping[next]; chained {
			return invalidf("Credential replacements must not contain chains or cycles")
		}
	}
	_, err := editConfiguration(s, func(state *State) (struct{}, Changes, error) {
		changes := Changes{}
		migrate := func(values []string) ([]string, bool) {
			out := append([]string{}, values...)
			for _, ref := range values {
				if next, ok := mapping[ref]; ok && !slices.Contains(out, next) {
					out = append(out, next)
				}
			}
			if complete {
				out = slices.DeleteFunc(out, func(ref string) bool { _, old := mapping[ref]; return old })
			}
			if slices.Equal(out, values) {
				return values, false
			}
			return out, true
		}
		rule := func(rule *RouteRule) bool {
			allow, changedAllow := migrate(rule.CredentialIDs)
			deny, changedDeny := migrate(rule.DeniedCredentialIDs)
			rule.CredentialIDs, rule.DeniedCredentialIDs = allow, deny
			return changedAllow || changedDeny
		}
		for i := range state.Routes {
			changes.Routes = rule(&state.Routes[i].Rule) || changes.Routes
		}
		for i := range state.Groups {
			changes.Groups = rule(&state.Groups[i].Rule) || changes.Groups
		}
		for scope, key := range state.Keys {
			if key != nil && rule(&key.RouteBindings.RouteRule) {
				changes.Keys = append(changes.Keys, scope)
			}
		}
		credentials := maps.Clone(state.ConfigCredentials)
		if credentials == nil {
			credentials = map[string]ConfigCredential{}
		}
		for old, next := range mapping {
			if credential, exists := credentials[old]; exists {
				if _, exists = credentials[next]; !exists {
					credentials[next] = credential
					changes.ConfigCredentials = true
				}
				if complete {
					delete(credentials, old)
					changes.ConfigCredentials = true
				}
			}
		}
		if changes.ConfigCredentials {
			state.ConfigCredentials = credentials
		}
		return struct{}{}, changes, nil
	})
	return err
}
