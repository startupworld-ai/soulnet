package peer

import (
	"context"
	"errors"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupDissolveStaged: a host with mixed-version members announces first
// (GroupDissolveNotify), kicks the legacy members one by one, then finishes with
// GroupDissolve. A node that already holds the "dissolved" marker keeps that reason when
// it is kicked afterwards; the final GroupDissolve on an owner-only roster still unpublishes.
func TestGroupDissolveStaged(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "staged", []string{bob.Fingerprint()}, a2a.DefaultGroupProfile())
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	if _, err := bob.GroupDissolveNotify(ctx, gid); !errors.Is(err, ErrGroupOwner) {
		t.Fatalf("bob announcing should fail with ErrGroupOwner, got %v", err)
	}
	st, err := alice.GroupDissolveNotify(ctx, gid)
	if err != nil {
		t.Fatalf("GroupDissolveNotify: %v", err)
	}
	if st == nil || alice.Groups.Get(gid) == nil {
		t.Fatal("notify alone must not forget the group")
	}
	bob.await(t, "bob sees the dissolution", func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonDissolved
	})

	// Legacy-style follow-up: kick bob. His reason must stay "dissolved".
	if err := alice.GroupKick(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatalf("GroupKick: %v", err)
	}
	if _, err := bob.GroupSend(ctx, gid, "still here?", GroupSendOptions{}); !errors.Is(err, ErrGroupLeft) {
		t.Fatalf("posting after the kick should fail with ErrGroupLeft, got %v", err)
	}
	if got := bob.GroupLeftReason(gid); got != "dissolved" {
		t.Fatalf("bob's reason after the kick = %q, want dissolved", got)
	}

	// Finish: owner-only roster, fan-out reaches nobody, roster comes off the relay.
	if err := alice.GroupDissolve(ctx, gid); err != nil {
		t.Fatalf("GroupDissolve: %v", err)
	}
	if alice.Groups.Get(gid) != nil {
		t.Fatal("owner still holds the dissolved group")
	}
	if _, err := alice.groupRelayClient(relayURL).FetchGroupCard(ctx, gid); err == nil {
		t.Fatal("relay still serves the group after unpublish")
	}
}
