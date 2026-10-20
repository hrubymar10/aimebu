package quota

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// The "same rules" acceptance test. testdata/watchdog-replay.json holds observations
// taken from the live out-of-tree watchdog this feature replaces: one record per
// distinct situation (profile statuses, session and weekly percentages bucketed
// around every limit in use, warm-up counters) out of ~5.6k ticks, with what the
// watchdog decided in that situation. Profile names are anonymised (alpha, bravo,
// charlie, and delta for the profile parked on purpose); only percentages and
// statuses are kept. The recorded decision is the watchdog's own: a switch, a hold
// (active snapshot not ok, warming up) or stay.
//
// The segment starts after the watchdog's last rule change, so the rules under test
// are: Claude orders alpha, bravo, charlie until alpha's weekly resets and bravo,
// alpha, charlie afterwards (the segment is entirely after the reset: the latch is
// set), 95/95 for every entry, delta protected; Codex alpha at session < 93 and
// weekly < 98, else bravo at < 98 / < 98, else stay put; nothing usable stays put;
// nothing is switched on an unclear active snapshot or while a profile settles.

type replayProfile struct {
	Status  string   `json:"status"`
	Session *float64 `json:"session"`
	Weekly  *float64 `json:"weekly"`
}

type replayRecord struct {
	Tool     string                   `json:"tool"`
	Active   string                   `json:"active"`
	Want     string                   `json:"want"`
	Profiles map[string]replayProfile `json:"profiles"`
	// Streak is the watchdog's count of consecutive ok polls per profile, capped at 6.
	Streak map[string]int `json:"streak"`
	// Action is what the watchdog did: stay, hold or switch; Reason is the hold
	// sentence, or "from>to" for a switch.
	Action string `json:"action"`
	Reason string `json:"reason"`
}

const watchdogPoll = 12 * time.Second // the watchdog polled every 12 s

func loadReplay(t *testing.T) map[string][]replayRecord {
	t.Helper()
	raw, err := os.ReadFile("testdata/watchdog-replay.json")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string][]replayRecord
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// replayConfig is the watchdog's rules as a Config. Warm-up is the watchdog's
// 3 consecutive polls (36 s) unless warmup says otherwise.
func replayConfig(tool string, warmup time.Duration) Config {
	var c Config
	switch tool {
	case ToolClaude:
		c = Default(ToolClaude, []string{"alpha", "bravo", "charlie"})
		c.Protected = []string{"delta"}
		c.AfterReset = &AfterReset{Profile: "alpha", DropPoints: 20, Profiles: []Limit{
			{Name: "bravo", SessionMax: 95, WeeklyMax: 95}, {Name: "alpha", SessionMax: 95, WeeklyMax: 95}, {Name: "charlie", SessionMax: 95, WeeklyMax: 95}}}
	default:
		c = Default(ToolCodex, []string{"alpha", "bravo"})
	}
	c.Enabled, c.DryRun = true, false
	c.WarmupSeconds = int(warmup / time.Second)
	c.MinDwellMinutes = 0 // the watchdog had no dwell
	return c
}

func replayInput(r replayRecord, cfg Config) Input {
	var obs []Obs
	for name, p := range r.Profiles {
		ok := p.Status == "ok"
		o := Obs{Name: name, Status: p.Status, Session: p.Session, Weekly: p.Weekly, HasCredentials: p.Status != "auth_missing"}
		if ok {
			o.OKFor = time.Duration(r.Streak[name]) * watchdogPoll
		}
		obs = append(obs, o)
	}
	state := State{}
	if r.Tool == ToolClaude { // the whole segment is after the weekly reset
		at := t0.Add(-time.Hour)
		peak := 84.0
		state.ResetLatchedAt, state.WeeklyMaxSeen = &at, &peak
	}
	return Input{Tool: r.Tool, Config: cfg, State: state, Profiles: obs, Active: r.Active, Now: t0, Migrated: true}
}

func (r replayRecord) describe() string {
	return fmt.Sprintf("%s active=%s watchdog=%s %s want=%s profiles=%v streak=%v", r.Tool, r.Active, r.Action, r.Reason, r.Want, r.Profiles, r.Streak)
}

func got(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// With the watchdog's own warm-up (3 polls) every one of its decisions is
// reproduced exactly: its switches (same from and to), its holds (same reason) and
// its stays.
func TestReplayOfTheWatchdogLogMatchesDecide(t *testing.T) {
	var counts = map[string]int{}
	for tool, records := range loadReplay(t) {
		cfg := replayConfig(tool, 3*watchdogPoll)
		for _, r := range records {
			d := Decide(replayInput(r, cfg)).Decision
			counts[tool+" "+r.Action]++
			switch r.Action {
			case "switch":
				if d.Action != ActionSwitch || got(d.From)+">"+got(d.To) != r.Reason {
					t.Errorf("watchdog switched %s, Decide says %s %s>%s (%s): %s", r.Reason, d.Action, got(d.From), got(d.To), d.Reason, r.describe())
				}
			case "hold":
				wantReason := map[string]string{"active snapshot not ok": ReasonActiveNotOK, "warming up": ReasonWarmingUp}[r.Reason]
				if wantReason == "" {
					t.Fatalf("fixture has an unmapped hold %q", r.Reason)
				}
				if d.Action != ActionHold || d.Reason != wantReason {
					t.Errorf("watchdog held (%s), Decide says %s %q: %s", r.Reason, d.Action, d.Reason, r.describe())
				}
			case "stay":
				if d.Action != ActionStay || got(d.To) != r.Active {
					t.Errorf("watchdog stayed on %s, Decide says %s %q to=%s: %s", r.Active, d.Action, d.Reason, got(d.To), r.describe())
				}
			default:
				t.Fatalf("fixture has an unknown action %q", r.Action)
			}
		}
	}
	// The fixture must keep exercising every kind of outcome in both tools.
	for _, k := range []string{"claude switch", "claude hold", "claude stay", "codex switch", "codex hold", "codex stay"} {
		if counts[k] == 0 {
			t.Errorf("the fixture has no %s record: %v", k, counts)
		}
	}
}

// The default warm-up is 60 s of continuous ok, not three polls. Against the same
// situations the only permitted difference is the documented one: Decide may hold
// for warm-up where the watchdog (3 polls = 36 s) had already acted; it never
// acts where the watchdog held, and never picks a different profile.
func TestReplayWithTheDefaultWarmupOnlyDiffersByWarmupHolds(t *testing.T) {
	differ := 0
	for tool, records := range loadReplay(t) {
		cfg := replayConfig(tool, DefaultWarmupSeconds*time.Second)
		for _, r := range records {
			d := Decide(replayInput(r, cfg)).Decision
			if r.Action == "hold" && d.Action != ActionHold {
				t.Errorf("the watchdog held (%s) but Decide acts (%s %s): %s", r.Reason, d.Action, d.Reason, r.describe())
			}
			if d.Action == ActionHold && d.Reason == ReasonWarmingUp && r.Action != "hold" {
				differ++
				continue
			}
			if r.Action == "switch" && (d.Action != ActionSwitch || got(d.From)+">"+got(d.To) != r.Reason) {
				t.Errorf("the watchdog switched %s, Decide says %s %s>%s: %s", r.Reason, d.Action, got(d.From), got(d.To), r.describe())
			}
			if r.Action == "stay" && d.Action != ActionStay && !(d.Action == ActionHold && d.Reason == ReasonWarmingUp) {
				t.Errorf("the watchdog stayed, Decide says %s %q: %s", d.Action, d.Reason, r.describe())
			}
		}
	}
	t.Logf("%d situations differ only by the longer warm-up", differ)
}
