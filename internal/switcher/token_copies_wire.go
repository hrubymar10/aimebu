package switcher

import (
	"log"
	"path/filepath"

	"github.com/hrubymar10/aimebu/internal/credtarget"
)

// newFanOut builds the live-target fan-out of a manager: the two live files
// (resolved by the manager itself, so both always agree) plus any externals in
// <config>/server/credential-targets.json. Every constructor that returns a
// manager able to mutate credentials installs it, so a CLI `switcher use` and
// the server switch through the same path and neither can reach a nil hook.
func newFanOut(m *Manager) *credtarget.FanOut {
	reg := credtarget.NewRegistry(filepath.Join(filepath.Dir(m.root), "server"), "")
	reg.LivePath = m.liveCredPath
	if err := reg.Load(); err != nil {
		log.Printf("credtarget: load target registry: %v", err)
	}
	return &credtarget.FanOut{
		Registry: reg,
		Writer:   &credtarget.Writer{},
		Active:   m.Active,
		WithLock: m.WithLock,
	}
}

// TokenCopyFanOut returns the manager's fan-out (nil on a read-only manager).
// The server shares it with the reconciler and the refresher.
func (m *Manager) TokenCopyFanOut() *credtarget.FanOut { return m.fanOut }
