package usages

import "errors"

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

// ErrTargetHoldsLogin is what a target writer returns when the file it was asked
// to rewrite still holds a refresh token: a /login landed there and has not been
// imported. It is not a failure of the refresher. Fan-out neither retries nor
// forces; the reconciler rescues, imports and strips the login. The credential
// target writer's own error wraps this one.
var ErrTargetHoldsLogin = errors.New("usages: a target holds a login that the reconciler has not imported yet")
