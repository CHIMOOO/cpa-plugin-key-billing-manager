package turnstate

// defaultsRevision 1 (v0.1.6 to v0.1.9) shipped a 240-second template lifetime
// renewed two minutes early. Revision 2 returns the 292 template to one hour:
// 240 seconds is the lifetime of the upstream cookies, which now have their
// own jar and their own setting.
const defaultsRevision = 2

// legacyConfig is the base a saved file is decoded over, so a setting the
// file omits keeps the value its writer used rather than a newer default.
func legacyConfig() Config {
	return DefaultConfig()
}

func shippedConfig() Config {
	return DefaultConfig()
}

// migrateDefaults undoes the revision 1 lifetime once. A lifetime the operator
// changed away from the revision 1 values is kept.
func migrateDefaults(state *diskState) bool {
	if state.Defaults >= defaultsRevision {
		return false
	}
	previous := state.Defaults
	state.Defaults = defaultsRevision
	if previous != 1 || state.Config.TTLSeconds != 240 || state.Config.RenewBeforeMinutes != 2 {
		return false
	}
	state.Config.TTLSeconds, state.Config.RenewBeforeMinutes = 3600, 0
	return true
}
