package credtarget

import (
	"errors"
	"time"
)

// MigrateMinLife is the C2 guard: migration refuses when a live token has less
// than this left, because a CLI mid-refresh in its last 5 minutes could spend
// the very token that is being moved into the store.
const MigrateMinLife = 15 * time.Minute

// ErrMigrateTokenExpiring is returned (CLI exit code 2) when the live token has
// less than MigrateMinLife left; nothing has been changed. The migration itself
// is switcher.Manager.MigrateToTokenCopies (it needs the switcher's locks and
// store, and credtarget must not import switcher).
var ErrMigrateTokenExpiring = errors.New("credtarget: live token expires too soon to migrate")
