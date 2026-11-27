package live

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hrubymar10/aimebu/v2/internal/eval"
)

// baseProject is what every case's checkout starts from: a small Go module that builds and passes its test, so a scenario about finished work
// finds a project, and its tests have something real to have passed on.
var baseProject = map[string]string{
	"go.mod":       "module example.invalid/eval\n\ngo 1.22\n",
	"main.go":      "package main\n\nimport \"fmt\"\n\nfunc greeting() string { return \"hello\" }\n\nfunc main() { fmt.Println(greeting()) }\n",
	"main_test.go": "package main\n\nimport \"testing\"\n\nfunc TestGreeting(t *testing.T) {\n\tif greeting() != \"hello\" {\n\t\tt.Fatal(\"greeting\")\n\t}\n}\n",
}

const baseTag = "eval-base"

// prepareCheckout puts the case's checkout in the state its scenario describes: the base project at the tag eval-base, then the scenario's commit
// (when it has one) on top. Whatever an earlier case left behind is removed first (reset and clean), except .aimebu, where a harness keeps
// its own state. The checkout is a git repository made by Options.checkout.
func prepareCheckout(dir string, want *eval.Checkout) error {
	if err := ensureScratchExclude(dir); err != nil {
		return err
	}
	git := func(args ...string) error {
		if out, err := gitCommand(dir, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return nil
	}
	write := func(files map[string]string) error {
		for name, content := range files {
			path := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	if gitCommand(dir, "rev-parse", "--verify", "-q", baseTag).Run() != nil {
		if err := write(baseProject); err != nil {
			return err
		}
		if err := git("add", "-A"); err != nil {
			return err
		}
		if err := git("commit", "-q", "-m", "initial project"); err != nil {
			return err
		}
		if err := git("tag", baseTag); err != nil {
			return err
		}
	}
	if err := git("reset", "-q", "--hard", baseTag); err != nil {
		return err
	}
	if err := git("clean", "-fdq", "-e", ".aimebu"); err != nil {
		return err
	}
	if want == nil {
		return nil
	}
	if err := write(want.Files); err != nil {
		return err
	}
	if err := git("add", "-A"); err != nil {
		return err
	}
	return git("commit", "-q", "-m", want.Commit)
}
