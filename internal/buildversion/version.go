package buildversion

import (
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/updatecheck"
)

// Older compares release tags carried by supervisor and server build strings.
// A describe-style development suffix identifies the same release tag, while
// unknown or unparseable builds never produce an outdated-build indication.
func Older(supervisor, server string) bool {
	parse := func(build string) (string, bool) {
		build = strings.TrimSpace(build)
		build = strings.TrimPrefix(build, "aimebu ")
		described, ok := updatecheck.ParseDescribe(build)
		return described.Tag, ok
	}
	left, leftOK := parse(supervisor)
	right, rightOK := parse(server)
	return leftOK && rightOK && updatecheck.CompareTags(left, right) < 0
}
