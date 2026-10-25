package credtarget

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// TargetWriter writes one target; *Writer implements it, tests use fakes.
type TargetWriter interface {
	WriteTarget(t Target, full []byte) error
}

// FanOut writes a fresh full credential to every target bound to the profile.
// It implements usages.TargetFanOut. Errors per target never abort the others;
// they are collected and returned joined, and the reconciler heals stragglers.
type FanOut struct {
	Registry *Registry
	Writer   TargetWriter
	// Active reports the active profile name of a tool ("" when none).
	Active func(tool string) (string, error)
	// WithLock runs fn under the switcher lock (switcher.Manager.WithLock). When
	// set, FanOut re-reads Active inside it, so a Switch that lands between the
	// refresher's decision and its write is never overwritten (a live target is
	// then no longer bound to the profile and is skipped: Switch already wrote the
	// right file). Nil means no locking (tests, single-writer callers).
	WithLock func(fn func() error) error
}

// FanOut rewrites the targets bound to (tool, profile) from full.
//
// Ordering rule: the caller commits the refreshed credential to the profile store
// FIRST; FanOut runs afterwards, so no target ever holds a token newer than the
// store.
func (f *FanOut) FanOut(tool, profile string, full []byte) error {
	if f.Registry == nil || f.Writer == nil || f.Active == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	run := func() error {
		active, err := f.Active(tool)
		if err != nil {
			return fmt.Errorf("credtarget: resolve active profile: %w", err)
		}
		return f.write(f.Registry.ForProfile(tool, profile, active != "" && active == profile), full)
	}
	if f.WithLock == nil {
		return run()
	}
	return f.WithLock(run)
}

// FanOutActive rewrites the live targets of tool from the (new) active
// profile's credential; used by Switch after (or just before) the pointer moves.
func (f *FanOut) FanOutActive(tool string, full []byte) error {
	if f.Registry == nil || f.Writer == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	return f.write(f.Registry.ActiveTargets(tool), full)
}

// ClearActive removes the credential file of every live target of tool: used
// when the active profile becomes an empty one, so no target keeps the outgoing
// account's token. A Claude target keeps its MCP OAuth logins (they are not the
// account's); it is removed only when it holds nothing else. A target that is
// already gone counts as cleared; errors are joined and never stop the others.
//
// A target that still holds a refresh token is a /login nobody imported yet:
// removing it would destroy the only copy. If any target holds one, nothing is
// removed and ErrRefreshTokenPresent is returned, so the caller refuses the
// switch and the reconciler imports the login first. A target that cannot be read
// at all is refused the same way.
func (f *FanOut) ClearActive(tool string) error {
	if f.Registry == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	targets := f.Registry.ActiveTargets(tool)
	for _, t := range targets {
		data, err := os.ReadFile(t.Path)
		switch {
		case err == nil && refreshTokenOnDisk(t.Tool, data):
			return fmt.Errorf("%w: %s", ErrRefreshTokenPresent, t.Path)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			// An unreadable target may hold a login: refuse rather than delete what cannot be inspected.
			return fmt.Errorf("%s: cannot check for an unimported login: %w", t.ID, err)
		}
	}
	clearer, _ := f.Writer.(targetClearer)
	var errs []error
	for _, t := range targets {
		var err error
		if clearer != nil {
			err = clearer.ClearTarget(t)
		} else if rmErr := os.Remove(t.Path); rmErr != nil && !os.IsNotExist(rmErr) {
			err = rmErr
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// targetClearer is a writer that removes the account login from a target while keeping what is not
// the login (Claude's MCP OAuth logins); *Writer does. Without it ClearActive removes the file.
type targetClearer interface {
	ClearTarget(t Target) error
}

// LoginReplacer is implemented by writers that can overwrite a target that still
// holds a refresh token (after the caller rescued and imported it); *Writer does.
type LoginReplacer interface {
	WriteImportedLogin(t Target, full []byte) error
}

// FanOutActiveReplacing is FanOutActive for the callers allowed to overwrite a
// refresh-bearing target: migrate-copies (the live full credential was just
// backed up and written into the store) and the reconciler's strip. The
// permission is per call (WriteImportedLogin), never a flag on the shared writer,
// and it covers only the host's live target, the one file those callers rescued.
// Any other target keeps the guard: a login sitting in an external follow-active
// target was never rescued, so it is skipped (the reconciler imports it) rather
// than overwritten.
func (f *FanOut) FanOutActiveReplacing(tool string, full []byte) error {
	if f.Registry == nil || f.Writer == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	replacer, ok := f.Writer.(LoginReplacer)
	if !ok {
		return errors.New("credtarget: the writer cannot replace a login")
	}
	var errs []error
	for _, t := range f.Registry.ActiveTargets(tool) {
		if t.Role == RoleLive {
			if err := replacer.WriteImportedLogin(t, full); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
			}
			continue
		}
		if err := f.Writer.WriteTarget(t, full); err != nil && !errors.Is(err, ErrRefreshTokenPresent) {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (f *FanOut) write(targets []Target, full []byte) error {
	var errs []error
	for _, t := range targets {
		if err := f.Writer.WriteTarget(t, full); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// PendingLogin is an external target that still holds an unimported login.
type PendingLogin struct {
	ID      string
	Profile string
	Data    []byte
}

// PendingLogins lists the registered external targets of tool that still hold a
// refresh token: a /login nobody imported yet. The host's live file is not
// listed (callers handle it themselves, with the rescue it needs).
func (f *FanOut) PendingLogins(tool string) []PendingLogin {
	if f.Registry == nil {
		return nil
	}
	var out []PendingLogin
	for _, t := range f.Registry.Targets() {
		if t.Tool != tool || t.Role == RoleLive {
			continue
		}
		if data, err := os.ReadFile(t.Path); err == nil && refreshTokenOnDisk(t.Tool, data) {
			out = append(out, PendingLogin{ID: t.ID, Profile: t.Profile, Data: data})
		}
	}
	return out
}

// guardedLoginReplacer is a writer that can re-check the target under its own write lock before replacing a login.
type guardedLoginReplacer interface {
	WriteImportedLoginIf(t Target, full []byte, guard LoginGuard) error
}

// sameLoginGuard lets a replacement through only while the target still holds exactly original: a removed target stays
// removed, one that is already token-only is left alone, and a different pair (the owner refreshed, rotated or logged in
// again) is left for the reconciler, so a newer login is never destroyed.
func sameLoginGuard(tool string, original []byte) LoginGuard {
	return func(existing []byte, exists bool) (bool, error) {
		switch {
		case !exists:
			return false, nil // a logout: stays removed
		case !HasRefreshToken(tool, existing):
			return false, nil // already token-only
		case !sameLogin(tool, original, existing):
			return false, errLoginMoved
		}
		return true, nil
	}
}

// StripLogin rewrites the registered target id token-only from full, replacing the login it holds. The caller has
// filed that login (original) in the profile store first, and holds the switcher lock. The check that the target still
// holds exactly that login runs under the writer's own lock, so a newer pair is never destroyed; one that has moved on
// is left for the reconciler (no error).
func (f *FanOut) StripLogin(tool, id string, original, full []byte) error {
	if f.Registry == nil || f.Writer == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	// Defence in depth: only ever rewrite a target with the credential of the family it held.
	if was, werr := ParseFull(tool, original); werr != nil || was.RefreshToken == "" {
		return fmt.Errorf("credtarget: the login to strip is not a full credential")
	} else if now, nerr := ParseFull(tool, full); nerr != nil || now.RefreshToken != was.RefreshToken {
		return fmt.Errorf("credtarget: refusing to rewrite target %s with a credential of another refresh-token family", id)
	}
	for _, t := range f.Registry.Targets() {
		if t.Tool != tool || t.ID != id || t.Role == RoleLive {
			continue
		}
		// A missing target is left missing: the guarded write would create its directory and take the storage lock first.
		if _, statErr := os.Stat(t.Path); errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
		var err error
		switch w := f.Writer.(type) {
		case guardedLoginReplacer:
			err = w.WriteImportedLoginIf(t, full, sameLoginGuard(tool, original))
		case LoginReplacer:
			// No lock-side check available: the best this writer allows is a read just before the write.
			current, readErr := os.ReadFile(t.Path)
			if readErr != nil || !HasRefreshToken(tool, current) || !sameLogin(tool, original, current) {
				return nil
			}
			err = w.WriteImportedLogin(t, full)
		default:
			return errors.New("credtarget: the writer cannot replace a login")
		}
		if errors.Is(err, errLoginMoved) {
			return nil
		}
		return err
	}
	return nil
}
