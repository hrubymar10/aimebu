package usages

// TargetFanOut distributes a freshly refreshed FULL credential to the token-only
// targets (the live files) bound to the profile. It is implemented by
// internal/credtarget.FanOut and injected by the server, which keeps this package
// free of an import cycle (switcher imports usages, credtarget imports both).
//
// The refresher calls it only AFTER the credential has been committed to the
// profile store, so no target ever holds a token newer than the store.
type TargetFanOut interface {
	// FanOut rewrites every target bound to (tool, profile) from full.
	FanOut(tool, profile string, full []byte) error
}

// SetTargetFanOut wires the fan-out. A nil fan-out (tests, switcher disabled)
// means the refresher only updates the store.
func (m *Manager) SetTargetFanOut(f TargetFanOut) { m.targetFanOut = f }
