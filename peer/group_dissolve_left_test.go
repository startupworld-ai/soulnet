package peer

import (
	"context"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupDissolveReachesRemovedMember: in a staged dissolution the kick can land before
// the member processes the dissolution notice (its roster refresh already sees a roster
// without it -> "removed"). The notice must still upgrade the marker to "dissolved"
// instead of being parked as a stray fan-out to a non-member.
func TestGroupDissolveReachesRemovedMember(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "raced", []string{bob.Fingerprint()}, a2a.DefaultGroupProfile())
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	// bob needs alice's sender key to open anything from her: one message settles that.
	if _, err := alice.GroupSend(ctx, gid, "hello", GroupSendOptions{}); err != nil {
		t.Fatalf("GroupSend: %v", err)
	}
	bob.await(t, "bob gets the message", func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid })

	// Simulate the race deterministically: bob already holds the removed marker when the
	// notice arrives (alice's roster still lists him, so the relay still fans out to him).
	bob.markGroupLeft(gid, "removed")
	if got := bob.GroupLeftReason(gid); got != "removed" {
		t.Fatalf("precondition: reason = %q, want removed", got)
	}
	if _, err := alice.GroupDissolveNotify(ctx, gid); err != nil {
		t.Fatalf("GroupDissolveNotify: %v", err)
	}
	bob.await(t, "bob sees the dissolution despite being marked removed", func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonDissolved
	})
	if got := bob.GroupLeftReason(gid); got != "dissolved" {
		t.Fatalf("reason after the notice = %q, want dissolved", got)
	}
	if rows := bob.GroupList(); len(rows) != 1 || rows[0].LeftReason != "dissolved" {
		t.Fatalf("list row should say dissolved, got %+v", rows)
	}
}
