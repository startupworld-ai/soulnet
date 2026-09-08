package peer

import (
	"context"
	"errors"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupDissolve: the owner dissolves a group → every member freezes it read-only with
// reason "dissolved" (archive kept, posting refused), the roster is gone from the relay,
// the owner no longer holds the group; a non-owner cannot dissolve.
func TestGroupDissolve(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)

	ctx := context.Background()
	profile := a2a.DefaultGroupProfile()
	profile.Public = true
	view, err := alice.GroupCreate(ctx, "short-lived", []string{bob.Fingerprint(), carol.Fingerprint()}, profile)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	if _, err := alice.GroupSend(ctx, gid, "one message before the end", GroupSendOptions{}); err != nil {
		t.Fatalf("GroupSend: %v", err)
	}
	bob.await(t, "bob gets the message", func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid })

	// A member cannot dissolve.
	if err := bob.GroupDissolve(ctx, gid); !errors.Is(err, ErrGroupOwner) {
		t.Fatalf("bob dissolving should fail with ErrGroupOwner, got %v", err)
	}

	if err := alice.GroupDissolve(ctx, gid); err != nil {
		t.Fatalf("GroupDissolve: %v", err)
	}
	// Owner side: gone.
	if alice.Groups.Get(gid) != nil {
		t.Fatal("owner still holds the dissolved group")
	}
	// Relay side: roster gone (fetch by a former member is 404, public card too).
	if _, err := alice.groupRelayClient(relayURL).FetchGroup(ctx, gid); err == nil {
		t.Fatal("relay still serves the roster after unpublish")
	}
	if _, err := alice.groupRelayClient(relayURL).FetchGroupCard(ctx, gid); err == nil {
		t.Fatal("relay still serves the public card after unpublish")
	}
	// Member side: read-only with reason dissolved, archive kept, posting refused.
	for _, m := range []*testNode{bob, carol} {
		m.await(t, a2a.ShortFp(m.Fingerprint())+" sees the dissolution", func(e Event) bool {
			return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonDissolved
		})
		if !m.GroupLeft(gid) || m.Groups.Get(gid) == nil {
			t.Fatalf("%s: group should stay on disk read-only", a2a.ShortFp(m.Fingerprint()))
		}
		if got := m.GroupLeftReason(gid); got != "dissolved" {
			t.Fatalf("%s: GroupLeftReason = %q, want dissolved", a2a.ShortFp(m.Fingerprint()), got)
		}
		if rows := m.GroupList(); len(rows) != 1 || !rows[0].Left || rows[0].LeftReason != "dissolved" {
			t.Fatalf("%s: list row should carry left + left_reason=dissolved, got %+v", a2a.ShortFp(m.Fingerprint()), rows)
		}
		if got := len(m.GroupConversation(gid, 0, 0)); got != 1 {
			t.Fatalf("%s: archive should keep the 1 message, got %d", a2a.ShortFp(m.Fingerprint()), got)
		}
		if _, err := m.GroupSend(ctx, gid, "anyone there?", GroupSendOptions{}); !errors.Is(err, ErrGroupLeft) {
			t.Fatalf("%s: posting to a dissolved group should fail with ErrGroupLeft, got %v", a2a.ShortFp(m.Fingerprint()), err)
		}
	}
	// A second dissolve is a clean "no such group".
	if err := alice.GroupDissolve(ctx, gid); !errors.Is(err, ErrNoGroup) {
		t.Fatalf("second dissolve: want ErrNoGroup, got %v", err)
	}
}
