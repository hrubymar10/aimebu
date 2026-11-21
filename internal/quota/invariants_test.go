package quota

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Invariants the owner asked to see pinned (not just implied by the policy table).

// A protected profile is never switched to and never switched away from, through
// the whole runner: a real (not dry-run) tool whose active profile is protected,
// with a listed profile that is perfectly usable, never reaches the actuator, and
// a protected profile that looks like the best target is never asked for.
func TestProtectedProfileIsNeverSwitchedToOrFromThroughTheRunner(t *testing.T) {
	h := newHarness(t)

	// c is parked on purpose and is the active profile; a is usable and listed.
	cfg := claudeCfg()
	cfg.Profiles = cfg.Profiles[:2] // a, b
	cfg.Protected = []string{"c"}
	cfg.WarmupSeconds, cfg.MinDwellMinutes = 0, 0
	h.save(ToolClaude, cfg)
	h.observe("c", ob("a", 1, 1), ob("b", 1, 1), ob("c", 99, 99))
	for i := 0; i < 5; i++ {
		h.tick(time.Minute)
	}
	if len(h.act.calls) != 0 {
		t.Fatalf("switched away from a protected active profile: %v", h.act.calls)
	}
	if action, reason := h.lastReason(); action != ActionHold || reason != ReasonProtected {
		t.Fatalf("decision = %s %q, want hold %q", action, reason, ReasonProtected)
	}

	// The active profile is exhausted and the only usable profile is protected (and
	// unlisted): nothing is switched to it.
	h.observe("b", ob("a", 99, 99), ob("b", 99, 99), ob("c", 1, 1))
	for i := 0; i < 5; i++ {
		h.tick(time.Minute)
	}
	if len(h.act.calls) != 0 {
		t.Fatalf("switched to a protected profile: %v", h.act.calls)
	}

	// Defence in depth: a hand-edited stored config that lists AND protects a profile
	// (Validate refuses it, but the policy does not rely on that) still never targets it.
	listed := claudeCfg()
	listed.WarmupSeconds, listed.MinDwellMinutes = 0, 0
	listed.Protected = []string{"a"} // a is first in the order and perfectly usable
	r := Decide(Input{Tool: ToolClaude, Config: listed, Profiles: []Obs{ob("a", 1, 1), ob("b", 99, 99), ob("c", 99, 99)}, Active: "b", Now: t0, Migrated: true})
	if r.Decision.Action == ActionSwitch && r.Decision.To != nil && *r.Decision.To == "a" {
		t.Fatalf("Decide targeted a protected profile: %+v", r.Decision)
	}

	// Protecting a profile that the order lists is refused at save, so a protected
	// profile can never be a listed target either.
	bad := claudeCfg()
	bad.Protected = []string{"a"}
	if _, err := h.r.SaveConfig(t.Context(), ToolClaude, bad); err == nil {
		t.Fatal("a listed profile can be saved as protected")
	}
}

// A switch only ever goes through the Actuator: the package imports no other part
// of the program (no switcher, credential, usage or store package) and calls no
// file-writing function, so it cannot write credential files or take the
// switcher's locks. The server's actuator is the one place that calls
// Switcher.Switch.
func TestQuotaPackageCannotWriteFilesOrTakeLocksOutsideTheActuator(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	forbidden := map[string]bool{"WriteFile": true, "Create": true, "CreateTemp": true, "OpenFile": true, "Rename": true,
		"Remove": true, "RemoveAll": true, "Mkdir": true, "MkdirAll": true, "Symlink": true, "Chmod": true, "Truncate": true}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "hrubymar10/aimebu/v2") {
				t.Errorf("%s imports %s: the quota policy must reach the program only through its interfaces", name, path)
			}
			if path == "syscall" || path == "golang.org/x/sys/unix" {
				t.Errorf("%s imports %s: no file locks here", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && forbidden[sel.Sel.Name] {
					t.Errorf("%s: os.%s: this package must not touch the file system", name, sel.Sel.Name)
				}
			}
			return true
		})
	}
}
