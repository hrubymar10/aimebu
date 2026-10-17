package credtarget

import (
	"errors"
	"fmt"
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
// account's token. A target that is already gone counts as cleared; errors are
// joined and never stop the others.
func (f *FanOut) ClearActive(tool string) error {
	if f.Registry == nil {
		return errors.New("credtarget: fan-out is not fully wired")
	}
	var errs []error
	for _, t := range f.Registry.ActiveTargets(tool) {
		if err := os.Remove(t.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// LoginReplacer is implemented by writers that can overwrite a target that still
// holds a refresh token (after the caller rescued and imported it); *Writer does.
type LoginReplacer interface {
	WriteImportedLogin(t Target, full []byte) error
}

// FanOutActiveReplacing is FanOutActive for the callers allowed to overwrite a
// refresh-bearing target: migrate-copies (the live full credential was just
// backed up and written into the store) and the reconciler's strip. The
// permission is per call (WriteImportedLogin), never a flag on the shared writer.
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
		if err := replacer.WriteImportedLogin(t, full); err != nil {
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
