package testenv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestMain(m *testing.M) { Main(m) }

func TestTheEnvironmentIsolateSetsUpPassesCheck(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AIMEBU_DEV_SKIP_V1_PROBE"); got != "1" {
		t.Errorf("test v1 listener probe setting = %q, want 1", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Variables {
		if name != "HOME" && !strings.HasPrefix(os.Getenv(name), home) {
			t.Errorf("%s = %q, want it under HOME %q", name, os.Getenv(name), home)
		}
	}
}

func TestCheckRefusesConfiguredServerURL(t *testing.T) {
	t.Setenv("AIMEBU_URL", "http://127.0.0.1:9997")
	if err := Check(); err == nil || !strings.Contains(err.Error(), "AIMEBU_URL") {
		t.Fatalf("Check accepted a configured server URL: %v", err)
	}
}

func TestIsolateCanonicalizesSymlinkedTemporaryDirectory(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(append([]string{}, Variables...), "TMPDIR") {
		t.Setenv(name, os.Getenv(name))
	}
	t.Setenv("TMPDIR", link)
	root, cleanup := Isolate()
	defer cleanup()
	if got := os.TempDir(); got != real {
		t.Errorf("temporary directory = %q, want %q", got, real)
	}
	if got := filepath.Dir(root); got != real {
		t.Errorf("test home parent = %q, want %q", got, real)
	}
	if got := os.Getenv("HOME"); got != root {
		t.Errorf("HOME = %q, want %q", got, root)
	}
}

func TestGoCachesArePinnedBeforeHomeChanges(t *testing.T) {
	for _, name := range []string{"GOCACHE", "GOPATH", "GOMODCACHE"} {
		if value := os.Getenv(name); value == "" {
			t.Errorf("%s is empty after Isolate", name)
		}
	}
}

func TestPinGoCacheEnvUsesEffectiveGoSettings(t *testing.T) {
	bin := t.TempDir()
	root := t.TempDir()
	goCommand := filepath.Join(bin, "go")
	script := "#!/bin/sh\n[ \"$*\" = 'env GOCACHE GOPATH GOMODCACHE' ] || exit 2\nprintf '%s\\n%s\\n%s\\n' '" + filepath.Join(root, "build") + "' '" + filepath.Join(root, "path") + "' '" + filepath.Join(root, "modules") + "'\n"
	if err := WriteExecutable(goCommand, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, name := range []string{"GOCACHE", "GOPATH", "GOMODCACHE"} {
		t.Setenv(name, "")
	}
	pinGoCacheEnv()
	for name, want := range map[string]string{"GOCACHE": filepath.Join(root, "build"), "GOPATH": filepath.Join(root, "path"), "GOMODCACHE": filepath.Join(root, "modules")} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestRestrictPATHMarksHarnessDiscoveryAsRestricted(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU_TEST_RESTRICT_PATH", "")
	t.Setenv("PATH", os.Getenv("PATH")) // RestrictPATH sets PATH itself; this registers its restore
	RestrictPATH(root, "go")
	if got := os.Getenv("PATH"); got != filepath.Join(root, "bin") {
		t.Fatalf("restricted PATH = %q", got)
	}
	if got := os.Getenv("AIMEBU_TEST_RESTRICT_PATH"); got != "1" {
		t.Fatalf("restricted harness discovery = %q", got)
	}
}

func TestCheckRefusesAnythingOutsideTheTemporaryDirectory(t *testing.T) {
	outside := "/" // the root is never under a temporary directory
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		outside = filepath.Dir(home)
	}
	for _, name := range Variables {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, outside)
			if err := Check(); err == nil {
				t.Fatalf("Check accepted %s=%q", name, outside)
			}
		})
	}
	t.Run("unset data directories are fine, they follow HOME", func(t *testing.T) {
		for _, name := range Variables[1:] {
			t.Setenv(name, "")
		}
		if err := Check(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCheckFollowsSymlinksAndRefusesATemporaryRootItself(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", link)
	if err := Check(); err != nil {
		t.Fatalf("a symlink into the temporary directory was refused: %v", err)
	}
	// A symlink out of it is where the path really is.
	out := filepath.Join(t.TempDir(), "out")
	if err := os.Symlink("/", out); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", out)
	if err := Check(); err == nil {
		t.Fatal("a symlink out of the temporary directory was accepted")
	}
	t.Setenv("CODEX_HOME", os.TempDir())
	if err := Check(); err == nil {
		t.Fatal("the temporary directory itself is shared with everything else and is not an isolated home")
	}
}

// Every package that has tests isolates them: it has a TestMain, and that TestMain calls into this package. A new package without one
// would run its tests against the real home of whoever runs them, which is how a test once waited for the real v1 server's lock.
func TestEveryPackageWithTestsIsolatesThem(t *testing.T) {
	root := moduleRoot(t)
	var missing []string
	dirs := packageTestFiles(t, root)
	if len(dirs) < 10 {
		t.Fatalf("found only %d packages with tests under %s: the walk is broken", len(dirs), root)
	}
	self, _ := os.Getwd() // this package's own TestMain is the one that calls Main without a qualifier
	for dir, files := range dirs {
		if dir != self && !declaresIsolatingTestMain(t, files) {
			rel, _ := filepath.Rel(root, dir)
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("packages with tests and no TestMain that calls testenv (add `func TestMain(m *testing.M) { testenv.Main(m) }`, or call testenv.Isolate in your own): %s", strings.Join(missing, ", "))
	}
}

// Git supplies both tracked tests and new, unignored tests. Review checkouts,
// build directories and other ignored files are not packages of this module.
func packageTestFiles(t *testing.T, root string) map[string][]string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go")
	cmd.Dir = root
	cmd.Env = cleanGitEnvironment()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("list module Go files: %v", err)
	}
	dirs := map[string][]string{}
	for _, rel := range strings.Split(string(output), "\x00") {
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		skip := false
		for _, part := range parts[:len(parts)-1] {
			if strings.HasPrefix(part, ".") || part == "testdata" || part == "node_modules" || part == "tmp" {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		path := filepath.Join(root, rel)
		if _, err := os.Stat(path); err != nil {
			continue // the index can still list a file deleted during local work
		}
		dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], path)
	}
	return dirs
}

func cleanGitEnvironment() []string {
	var env []string
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			env = append(env, variable)
		}
	}
	return env
}

func TestPackageTestFilesSkipsHiddenAndGitIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	hiddenPath := filepath.Join(".rag"+"nar-cr", "x/internal/foo/foo_test.go")
	t.Setenv("GIT_DIR", filepath.Join(root, "foreign.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(root, "foreign-index"))
	cmd := exec.Command("git", "init", "-q", root)
	cmd.Env = cleanGitEnvironment()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	for path, body := range map[string]string{
		".gitignore":                 "ignored/\n",
		"internal/good/good_test.go": "package good\n",
		"internal/gone/gone_test.go": "package gone\n",
		hiddenPath:                   "package foo\n",
		"ignored/hidden_test.go":     "package ignored\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	add := exec.Command("git", "add", "internal/gone/gone_test.go")
	add.Dir = root
	add.Env = cleanGitEnvironment()
	if output, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	if err := os.Remove(filepath.Join(root, "internal/gone/gone_test.go")); err != nil {
		t.Fatal(err)
	}
	dirs := packageTestFiles(t, root)
	if len(dirs) != 1 || len(dirs[filepath.Join(root, "internal/good")]) != 1 {
		t.Fatalf("found test packages %v, want only internal/good", dirs)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's directory")
		}
		dir = parent
	}
}

// declaresIsolatingTestMain parses the test files of one package directory for a TestMain whose body calls a function of this package.
func declaresIsolatingTestMain(t *testing.T, files []string) bool {
	t.Helper()
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		alias := ""
		for _, spec := range parsed.Imports {
			if strings.Trim(spec.Path.Value, `"`) == "github.com/hrubymar10/aimebu/v2/internal/testenv" {
				alias = "testenv"
				if spec.Name != nil {
					alias = spec.Name.Name
				}
			}
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "TestMain" || fn.Recv != nil || fn.Body == nil || alias == "" {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
						if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == alias {
							found = true
						}
					}
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

func TestIsolateForcesErrorReportingOff(t *testing.T) {
	if got := os.Getenv("AIMEBU_SENTRY_DSN"); got != "off" {
		t.Fatalf("the test environment must force AIMEBU_SENTRY_DSN=off, got %q", got)
	}
	t.Setenv("AIMEBU_SENTRY_DSN", "https://k@h.example/1")
	if Check() == nil {
		t.Fatal("Check must refuse a test environment where error reporting is not off")
	}
}

// A test that writes a file to run with os.WriteFile and an executable mode can fail with "text file busy" whenever another goroutine forks while the file is
// open for writing (or when it rewrites a launcher a process is running): it writes with WriteExecutable instead.
func TestNoTestWritesAnExecutableWithWriteFile(t *testing.T) {
	if found := executableWriteFileUses(moduleRoot(t)); len(found) > 0 {
		t.Fatalf("use testenv.WriteExecutable instead of os.WriteFile with an executable mode (it fails with ETXTBSY under parallel forks): %s", strings.Join(found, ", "))
	}
}

// executableWriteFileUses lists the test files under root (as path:line) that write an executable with os.WriteFile. It does not enter a directory whose name
// starts with a dot (review copies, agent worktrees, .git: none is part of this module) or node_modules.
func executableWriteFileUses(root string) []string {
	exec := regexp.MustCompile(`os\.WriteFile\([^\n]*, 0o?7(00|55)\)`)
	var found []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for number, line := range strings.Split(string(data), "\n") {
			if exec.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				found = append(found, fmt.Sprintf("%s:%d", rel, number+1))
			}
		}
		return nil
	})
	return found
}

// Directories that are not part of the module are not scanned, however many of them sit under the root; a real offender under internal/ still is.
func TestExecutableWriteScanSkipsDotDirectories(t *testing.T) {
	root := t.TempDir()
	bad := "package x\nfunc f() { _ = os.Write" + "File(p, b, 0" + "700) }\n"
	for _, dir := range []string{".review-cache/review/internal/x", ".aimebu/worktrees/x/internal/y", "web/node_modules/z"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "old_test.go"), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if found := executableWriteFileUses(root); len(found) != 0 {
		t.Fatalf("skipped directories were scanned: %v", found)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal/real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal/real/bad_test.go"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if found := executableWriteFileUses(root); len(found) != 1 || found[0] != filepath.Join("internal", "real", "bad_test.go")+":2" {
		t.Fatalf("found %v, want the one under internal/", found)
	}
}

// WriteExecutable's file runs straight away, rewriting it while it runs works, and doing both from many goroutines that also start processes never fails.
func TestWriteExecutableNeverRaces(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			path := filepath.Join(dir, fmt.Sprintf("script-%d", worker))
			for round := 0; round < 25; round++ {
				if err := WriteExecutable(path, []byte(fmt.Sprintf("#!/bin/sh\necho %d\n", round)), 0700); err != nil {
					errs <- err
					return
				}
				out, err := exec.Command(path).Output()
				if err != nil || strings.TrimSpace(string(out)) != fmt.Sprint(round) {
					errs <- fmt.Errorf("worker %d round %d: %q %v", worker, round, out, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
