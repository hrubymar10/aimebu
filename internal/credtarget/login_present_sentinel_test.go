package credtarget

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hrubymar10/aimebu/internal/usages"
)

// The refresher recognises a login waiting in a target through
// usages.ErrTargetHoldsLogin (usages cannot import this package). Fan-out joins
// per-target errors, so the match must survive wrapping and joining.
func TestRefreshTokenPresentIsRecognisedByTheRefresher(t *testing.T) {
	if !errors.Is(ErrRefreshTokenPresent, usages.ErrTargetHoldsLogin) {
		t.Fatal("ErrRefreshTokenPresent must wrap usages.ErrTargetHoldsLogin")
	}
	joined := errors.Join(fmt.Errorf("live/claude: %w", fmt.Errorf("%w: /x/.credentials.json", ErrRefreshTokenPresent)), errors.New("other"))
	if !errors.Is(joined, usages.ErrTargetHoldsLogin) || !errors.Is(joined, ErrRefreshTokenPresent) {
		t.Fatal("the sentinel was lost through wrapping or errors.Join")
	}
}
