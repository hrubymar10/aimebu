package configroot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, want string }{
		{"", filepath.Join(home, ".aimebu")},
		{"~", home},
		{"~/x", filepath.Join(home, "x")},
		{filepath.Join(home, "data"), filepath.Join(home, "data")},
	} {
		got, err := Resolve(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, raw := range []string{"data", "./data", "~other/data"} {
		if _, err := Resolve(raw); err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
			t.Errorf("Resolve(%q) error = %v", raw, err)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", "~/data")
	want, err := Resolve("~/data")
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromEnv()
	if err != nil || got != want {
		t.Fatalf("FromEnv() = %q, %v; want %q", got, err, want)
	}
}
