package peer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// The forget-on-403 branch must key on the STATUS, not on message text: the
// production relay localizes its errors (Chinese), so any substring match on
// English is permanently dead there. Caught live by the three-machine matrix:
// a kicked member never forgot the group.
func TestRelayForbiddenByStatusNotText(t *testing.T) {
	zh := &a2a.RelayError{StatusCode: 403, Message: "不是该群成员"}
	if !relayForbidden(zh) {
		t.Fatal("403 with a localized message must count as forbidden")
	}
	if !relayForbidden(fmt.Errorf("fetch: %w", zh)) {
		t.Fatal("wrapped RelayError must unwrap")
	}
	if relayForbidden(&a2a.RelayError{StatusCode: 500, Message: "boom"}) {
		t.Fatal("a 500 is not a membership verdict")
	}
	if relayForbidden(errors.New("relay 403: not a member")) {
		t.Fatal("plain text mentioning 403 must NOT be trusted")
	}
}
