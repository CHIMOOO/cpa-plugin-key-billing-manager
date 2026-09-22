package turnstate

// defaultsRevision 1 shortened the shipped template lifetime from one hour to
// 240 seconds with renewal two minutes before expiry.
const defaultsRevision = 1

// The lifetime a new or migrated state file starts with. Package tests keep
// the one-hour DefaultConfig their fixtures were written against.
var shippedTTLSeconds, shippedRenewBeforeMinutes = 240, 2

// legacyConfig is the base a saved file is decoded over, so a setting the
// file omits keeps the value its writer used rather than a newer default.
func legacyConfig() Config {
	return DefaultConfig()
}

func shippedConfig() Config {
	cfg := DefaultConfig()
	cfg.TTLSeconds, cfg.RenewBeforeMinutes = shippedTTLSeconds, shippedRenewBeforeMinutes
	return cfg
}

// migrateDefaults moves the old one-hour lifetime to the current default once.
// A lifetime the operator saves later is kept, including a return to 3600.
func migrateDefaults(state *diskState) bool {
	if state.Defaults >= defaultsRevision {
		return false
	}
	state.Defaults = defaultsRevision
	if state.Config.TTLSeconds != 3600 || shippedTTLSeconds == 3600 {
		return false
	}
	state.Config.TTLSeconds, state.Config.RenewBeforeMinutes = shippedTTLSeconds, shippedRenewBeforeMinutes
	return true
}
