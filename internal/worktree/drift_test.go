package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// driftRepo is a repository whose branch has moved n commits past the returned base commit, the newest subject being "commit n".
func driftRepo(t *testing.T, n int, subject func(i int) string) (dir, base string) {
	t.Helper()
	dir = repoFixture(t)
	base = runGit(t, dir, "rev-parse", "HEAD")
	for i := 1; i <= n; i++ {
		runGit(t, dir, "commit", "-q", "--allow-empty", "-m", subject(i))
	}
	return dir, base
}

func numbered(i int) string { return fmt.Sprintf("commit %d", i) }

func TestBaseDriftBelowThresholdSaysNothingMore(t *testing.T) {
	dir, base := driftRepo(t, 4, numbered)
	got, err := BaseDrift(context.Background(), dir, "HEAD", base, 5, 5)
	if err != nil || got.Behind != 4 || len(got.Subjects) != 0 {
		t.Fatalf("drift = %+v, %v; want 4 behind and no subjects", got, err)
	}
}

func TestBaseDriftAtAndAboveThresholdListsNewestSubjects(t *testing.T) {
	for _, n := range []int{5, 8} {
		dir, base := driftRepo(t, n, numbered)
		got, err := BaseDrift(context.Background(), dir, "HEAD", base, 5, 5)
		if err != nil || got.Behind != n || len(got.Subjects) != 5 {
			t.Fatalf("n=%d: drift = %+v, %v", n, got, err)
		}
		if got.Subjects[0] != fmt.Sprintf("commit %d", n) || got.Subjects[4] != fmt.Sprintf("commit %d", n-4) {
			t.Fatalf("n=%d: subjects = %q, want the 5 newest first", n, got.Subjects)
		}
	}
}

func TestBaseDriftConfiguredThreshold(t *testing.T) {
	dir, base := driftRepo(t, 2, numbered)
	got, err := BaseDrift(context.Background(), dir, "HEAD", base, 2, 5)
	if err != nil || got.Behind != 2 || len(got.Subjects) != 2 {
		t.Fatalf("drift = %+v, %v; want 2 behind with 2 subjects at a threshold of 2", got, err)
	}
}

func TestBaseDriftMissingRefIsUnknown(t *testing.T) {
	dir, base := driftRepo(t, 6, numbered)
	for _, ref := range []string{"no-such-branch", "", "-x", "bad\nref"} {
		if _, err := BaseDrift(context.Background(), dir, ref, base, 5, 5); !errors.Is(err, ErrNoDrift) {
			t.Fatalf("ref %q: err = %v, want ErrNoDrift", ref, err)
		}
	}
	if _, err := BaseDrift(context.Background(), dir, "HEAD", "not-a-commit", 5, 5); !errors.Is(err, ErrNoDrift) {
		t.Fatalf("bad base commit: err = %v, want ErrNoDrift", err)
	}
	if _, err := BaseDrift(context.Background(), t.TempDir(), "HEAD", base, 5, 5); !errors.Is(err, ErrNoDrift) {
		t.Fatalf("not a repository: err = %v, want ErrNoDrift", err)
	}
}

func TestBaseDriftSanitisesSubjects(t *testing.T) {
	odd := func(i int) string {
		switch i {
		case 1:
			return "tab\there \x1b[31mred\x1b[0m"
		case 2:
			return "zero​width ‮flip"
		case 3:
			return strings.Repeat("long ", 100)
		default:
			return "plain"
		}
	}
	dir, base := driftRepo(t, 5, odd)
	got, err := BaseDrift(context.Background(), dir, "HEAD", base, 5, 5)
	if err != nil || len(got.Subjects) != 5 {
		t.Fatalf("drift = %+v, %v", got, err)
	}
	for _, s := range got.Subjects {
		for _, r := range s {
			if r < 0x20 || r == 0x7f || r == 0x200b || r == 0x202e {
				t.Fatalf("subject %q keeps control character %U", s, r)
			}
		}
		if len([]rune(s)) > maxDriftSubjectRunes+1 {
			t.Fatalf("subject has %d runes, cap is %d", len([]rune(s)), maxDriftSubjectRunes)
		}
	}
	if got.Subjects[4] != "tab here [31mred[0m" || got.Subjects[3] != "zerowidth flip" {
		t.Fatalf("subjects = %q", got.Subjects)
	}
}
