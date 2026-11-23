package server

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// codexOverwriteChecks are when, counted from the switch, the live auth.json of Codex is looked at again on a timer. Between and after them it
// is looked at on every usage refresh and before every Codex turn on the host (see checkCodexOverwrite).
var codexOverwriteChecks = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

// codexWatch is what the Codex overwrite check of one switch expects in the live auth.json: the profile the switch installed and its account,
// taken from what the switch wrote (switcher.LastInstalled), never from re-reading the live file, which a running codex may already have
// overwritten. It lasts until the next Codex switch replaces it.
type codexWatch struct {
	profile string
	account string
	kick    chan struct{} // a usage refresh asks for a check (never blocks: one pending kick is enough)
	stop    chan struct{} // closed when the next switch replaces the watch

	mu       sync.Mutex // one check at a time; guards snapshot and raised
	snapshot []byte     // what is installed again: the newest live bytes seen naming the account (fresher tokens than the switch's)
	raised   bool       // the one switch_overwritten event of this switch was raised
}

// watchCodexOverwrite starts the overwrite check of the Codex switch just made. A codex process that was running at the switch can write its own
// tokens, an old account's, back into auth.json (an in-flight persist_tokens takes no lock). When the file names another stored profile's account
// instead of the one the switch installed, the installed login is put back, and one switch_overwritten event is raised for the owner however
// many checks had to.
func (s *Server) watchCodexOverwrite() {
	installed, ok := s.Switcher.LastInstalled(switcher.ToolCodex)
	if !ok || installed.Account == "" || len(installed.Data) == 0 {
		s.replaceCodexWatch(nil)
		return
	}
	w := &codexWatch{profile: installed.Profile, account: installed.Account, snapshot: installed.Data,
		kick: make(chan struct{}, 1), stop: make(chan struct{})}
	s.replaceCodexWatch(w)
	ctx := s.switchBackgroundContext()
	after := s.switchCheckAfter
	if after == nil {
		after = time.After
	}
	s.switchBg.Add(1)
	telemetry.Go(context.Background(), func() {
		defer s.switchBg.Done()
		next := after(codexOverwriteChecks[0])
		done := 0
		for {
			select {
			case <-next:
				done++
				next = nil
				if done < len(codexOverwriteChecks) {
					next = after(codexOverwriteChecks[done] - codexOverwriteChecks[done-1])
				}
			case <-w.kick:
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			}
			s.checkCodexWatch(w)
		}
	})
	// The first check is now: a codex process may have written the old account back already, before the switch returned.
	s.checkCodexWatch(w)
}

func (s *Server) replaceCodexWatch(w *codexWatch) {
	if old := s.codexWatch.Swap(w); old != nil {
		close(old.stop)
	}
}

// kickCodexOverwrite asks the running overwrite check to look again, without waiting for it: a usage refresh calls it while the usage reader
// holds its lock.
func (s *Server) kickCodexOverwrite() {
	if w := s.codexWatch.Load(); w != nil {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// checkCodexOverwrite looks at the live Codex login now, before a Codex turn starts on the host, so the turn starts on the installed login.
func (s *Server) checkCodexOverwrite() {
	if w := s.codexWatch.Load(); w != nil {
		s.checkCodexWatch(w)
	}
}

// checkCodexWatch is one check. The usual case is cheap: the live file is read and its account id compared with the expected one; only a
// different account takes the switcher lock (ReinstallIfOverwritten), which installs the snapshot again when the file names an account another
// stored profile holds.
func (s *Server) checkCodexWatch(w *codexWatch) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.codexWatch.Load() != w {
		return // a later switch replaced it
	}
	data, account, err := s.Switcher.LiveAccount(switcher.ToolCodex)
	if err != nil || account == "" {
		return
	}
	if account == w.account {
		w.snapshot = data // codex refreshes rotate the tokens: put back the newest ones, not the switch's
		return
	}
	reinstalled, err := s.Switcher.ReinstallIfOverwritten(switcher.ToolCodex, w.profile, w.account, w.snapshot)
	if err != nil {
		log.Printf("switcher: codex: the check after the switch failed: %v", err)
		return
	}
	if !reinstalled {
		return
	}
	log.Printf("switcher: codex: a running session wrote an old login back after the switch; %s was installed again", w.profile)
	if w.raised {
		return
	}
	w.raised = true
	eventCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.RecordQuotaEvent(eventCtx, "switch_overwritten", map[string]string{"tool": switcher.ToolCodex}); err != nil {
		log.Printf("switcher: codex: recording the switch_overwritten event: %v", err)
	}
}
