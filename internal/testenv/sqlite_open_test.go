package testenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These owners open their own SQLite files. A CLI process must not open the
// server database while its writer is running, even with mode=ro.
var sqliteOpenAllowlist = map[string]string{
	"cmd/aimebu/doctor_db.go:quickCheckBackup":  "checks an immutable migration backup, never the live store",
	"cmd/aimebu/migration_v1.go:backupV1Layout": "moves a legacy v1 source; a root server path first takes the v2 startup lock",
	"cmd/aimebu/migration_v1.go:openV1Database": "identifies a legacy v1 source; v2 server paths are inspected under the startup lock",
}

func TestNoSecondProcessOpensAimebuDatabase(t *testing.T) {
	// Test files are excluded: the package TestMain isolates HOME and all
	// config roots, so their fixture databases cannot be real user data.
	root := moduleRoot(t)
	cmd := exec.Command("git", "ls-files", "--cached", "-z", "--", "cmd/**/*.go", "internal/**/*.go")
	cmd.Dir = root
	cmd.Env = cleanGitEnvironment()
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range strings.Split(string(output), "\x00") {
		if path == "" || strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "internal/store/") || strings.HasPrefix(path, "internal/supervisor/") {
			continue
		}
		var source any
		if ref := os.Getenv("AIMEBU_SQLITE_SCAN_REF"); ref != "" && path == "cmd/aimebu/doctor.go" {
			show := exec.Command("git", "show", ref+":"+path)
			show.Dir = root
			show.Env = cleanGitEnvironment()
			data, err := show.Output()
			if err != nil {
				t.Fatal(err)
			}
			source = data
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), source, 0)
		if err != nil {
			t.Fatal(err)
		}
		sqlNames := map[string]bool{}
		dsnNames := map[string]bool{}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(importPath)
			if imported.Name != nil {
				name = imported.Name.Name
			}
			if importPath == "database/sql" {
				sqlNames[name] = true
			}
			if strings.HasSuffix(importPath, "/internal/sqliteutil") {
				dsnNames[name] = true
			}
		}
		inspect := func(node ast.Node, key string) {
			ast.Inspect(node, func(n ast.Node) bool {
				switch call := n.(type) {
				case *ast.CallExpr:
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if ident, ok := sel.X.(*ast.Ident); ok {
							if sqlNames[ident.Name] && sel.Sel.Name == "Open" && sqliteOpenAllowlist[key] == "" {
								t.Errorf("%s: sql.Open outside database owner", key)
							}
							if dsnNames[ident.Name] && sel.Sel.Name == "DSN" && len(call.Args) == 2 && sqliteOpenAllowlist[key] == "" {
								if readOnly, ok := call.Args[1].(*ast.Ident); ok && readOnly.Name == "true" {
									t.Errorf("%s: read-only SQLite DSN outside database owner", key)
								}
							}
						}
					}
				case *ast.BasicLit:
					if strings.Contains(strings.ToLower(call.Value), "mode=ro") && sqliteOpenAllowlist[key] == "" {
						t.Errorf("%s: read-only SQLite DSN outside database owner", key)
					}
				}
				return true
			})
		}
		for _, declaration := range file.Decls {
			switch item := declaration.(type) {
			case *ast.FuncDecl:
				inspect(item.Body, path+":"+item.Name.Name)
			case *ast.GenDecl:
				inspect(item, path+":<global>")
			}
		}
	}
}
