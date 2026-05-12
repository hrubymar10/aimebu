package bin_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAimebuWrapper(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(file), "aimebu_test.sh")
	cmd := exec.Command("bash", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper smoke test failed: %v\n%s", err, output)
	}
}
