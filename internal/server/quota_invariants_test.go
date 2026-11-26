package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// The quota switcher's server side may only read the switcher (list profiles,
// overlay usage, ask whether a tool is migrated, read the durable manual
// choice) and switch through
// Switcher.SwitchGuarded (never the unguarded Switch: the guard is what keeps a
// switch from overriding one the owner made in the meantime). Any other Switcher method here, and any direct file write, would
// be a new way to change credentials or take the switcher's locks.
func TestQuotaServerSideOnlyReadsTheSwitcherAndSwitches(t *testing.T) {
	allowed := map[string]bool{"List": true, "OverlayUsages": true, "TokenCopiesFor": true, "ManualChoice": true, "SwitchGuarded": true}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "quota.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// x.Switcher.<Method>
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Switcher" {
			used[sel.Sel.Name] = true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" {
			t.Errorf("quota.go calls os.%s: the server side must not touch the file system", sel.Sel.Name)
		}
		return true
	})
	var extra []string
	for name := range used {
		if !allowed[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("quota.go calls Switcher methods beyond List/OverlayUsages/TokenCopiesFor/ManualChoice/SwitchGuarded: %v", extra)
	}
	if !used["SwitchGuarded"] {
		t.Error("quota.go no longer switches through Switcher.SwitchGuarded")
	}
}
