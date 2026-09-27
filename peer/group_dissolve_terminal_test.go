package peer

import (
	"context"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupDissolvedIsTerminal: in a staged dissolution the owner announces first and
// then kicks members one by one, so the relay serves newer roster versions that still
// list a member who already froze the group as "dissolved". Refetching such a roster
// must not read as a re-admission: the marker stays, the group stays read-only. (Before
// the fix the marker was cleared, and once the roster came off the relay (404) nothing
// froze the group again - it looked alive forever.)
func TestGroupDissolvedIsTerminal(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "terminal", []string{bob.Fingerprint(), carol.Fingerprint()}, a2a.DefaultGroupProfile())
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	if _, err := alice.GroupDissolveNotify(ctx, gid); err != nil {
		t.Fatalf("GroupDissolveNotify: %v", err)
	}
	bob.await(t, "bob sees the dissolution", func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonDissolved
	})
	before := bob.Groups.Get(gid).Roster.Version

	// Staged step: kick carol first. The relay now serves a NEWER roster that still lists bob.
	if err := alice.GroupKick(ctx, gid, carol.Fingerprint()); err != nil {
		t.Fatalf("GroupKick: %v", err)
	}
	fetched, err := bob.groupRelayClient(relayURL).FetchGroup(ctx, gid)
	if err != nil {
		t.Fatalf("FetchGroup: %v", err)
	}
	if fetched.Version <= before || fetched.Member(bob.Fingerprint()) == nil {
		t.Fatalf("precondition: relay roster v%d (bob held v%d) should be newer and still list bob", fetched.Version, before)
	}

	// Any path that refetches (a group_update, a group_key from a "non-member", an invite)
	// ends in applyRoster; drive it directly so the race is deterministic.
	bob.refreshRoster(ctx, gid)
	if got := bob.GroupLeftReason(gid); got != "dissolved" {
		t.Fatalf("after refetching a roster that still lists bob, reason = %q, want dissolved", got)
	}
	if !bob.GroupLeft(gid) {
		t.Fatal("a dissolved group came back to life")
	}
	// A replayed invite carrying that roster is no re-admission either.
	if applied := bob.applyRoster(ctx, bob.Groups.Get(gid), fetched); applied || bob.GroupLeftReason(gid) != "dissolved" {
		t.Fatalf("applyRoster on a dissolved group: applied=%v reason=%q", applied, bob.GroupLeftReason(gid))
	}
}
