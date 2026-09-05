package peer

import (
	"context"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// cardsInArchive counts the archived group entries that carry a sender card.
func cardsInArchive(n *Peer, gid string) int {
	k := 0
	for _, e := range n.GroupConversation(gid, 0, 0) {
		if e.Card != nil {
			k++
		}
	}
	return k
}

// renameNode gives a node a new nickname the way a host that owns identity.json does:
// same keys, new name, re-signed card from now on.
func renameNode(n *Peer, name string) {
	renamed := *n.Identity()
	renamed.Name = name
	n.SetIdentity(&renamed)
}

// TestGroupRenamePropagatesThroughPosts: a member's re-signed card rides on its next
// group post exactly once per co-member, a friend's snapshot and note follow, a
// non-friend learns the name from the announced card, and the owner converges the
// roster (version+1, once) so the roster copy follows too. Nothing extra goes out for
// posts after that.
func TestGroupRenamePropagatesThroughPosts(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice") // owner
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	// bob and carol are NOT friends: carol can only learn bob's new name through the group.

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "rename club", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid, v0 := view.GID, view.Version
	bobFp := bob.Fingerprint()
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	post := func(body string) {
		t.Helper()
		if res, err := bob.GroupSend(ctx, gid, body, GroupSendOptions{}); err != nil || res.Status != "sent" {
			t.Fatalf("GroupSend %q: %+v %v", body, res, err)
		}
		for _, r := range []*testNode{alice, carol} {
			r.await(t, r.Identity().Name+" gets "+body, func(e Event) bool {
				return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == body
			})
		}
	}

	// Two plain posts: the first carries bob's card (nobody received it through this
	// group yet), the second does not - and an unchanged card never republishes anything.
	post("one")
	post("two")
	if got := cardsInArchive(alice.Peer, gid); got != 1 {
		t.Fatalf("alice archived %d posts with a card, want exactly 1 (first post only)", got)
	}
	if got := alice.Groups.Get(gid).Roster.Version; got != v0 {
		t.Fatalf("roster moved to v%d without any rename (want v%d)", got, v0)
	}
	if got := len(alice.Groups.MemberCards(gid)); got != 0 {
		t.Fatalf("an unchanged card must not be cached as announced, got %d", got)
	}

	// bob renames and posts once more.
	renameNode(bob.Peer, "Bobby")
	post("three")
	waitUntil(t, "alice shows Bobby (friend snapshot refreshed)", func() bool {
		return alice.GroupNames(gid, []string{bobFp})[bobFp] == "Bobby"
	})
	if fr := alice.Friends.Get(bobFp); fr.Note != "Bobby" || fr.Card.Name != "Bobby" {
		t.Fatalf("alice's friend record after rename: note=%q card=%q (note was never hand-edited, must follow)", fr.Note, fr.Card.Name)
	}
	waitUntil(t, "carol shows Bobby (not a friend: announced card or converged roster)", func() bool {
		return carol.GroupNames(gid, []string{bobFp})[bobFp] == "Bobby"
	})
	waitUntil(t, "the owner converged the roster and carol received it", func() bool {
		st := carol.Groups.Get(gid)
		return st != nil && st.Roster.Version == v0+1 && st.Roster.Member(bobFp) != nil && st.Roster.Member(bobFp).Name == "Bobby"
	})
	waitUntil(t, "carol's announced copy is pruned once the roster carries the card", func() bool {
		return len(carol.Groups.MemberCards(gid)) == 0
	})
	if carol.GroupNames(gid, []string{bobFp})[bobFp] != "Bobby" {
		t.Fatal("carol must still show Bobby from the roster after the prune")
	}

	// One more post: everyone holds this version - no card, no roster movement.
	post("four")
	if got := cardsInArchive(alice.Peer, gid); got != 2 {
		t.Fatalf("alice archived %d posts with a card, want 2 (one per card version)", got)
	}
	if got := alice.Groups.Get(gid).Roster.Version; got != v0+1 {
		t.Fatalf("roster republished more than once for the same card: v%d (want v%d)", got, v0+1)
	}
	for _, e := range bob.GroupConversation(gid, 0, 0) {
		if e.Card != nil {
			t.Fatal("the sender's own archive must not store its card")
		}
	}
}

// TestAbsorbGroupCardGates: a card on a group post is taken only when it verifies, is the
// sender's own, and the sender is on the roster; a genuine one converges the owner's
// roster exactly once per card version.
func TestAbsorbGroupCardGates(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "gates", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid, v0 := view.GID, view.Version
	bobFp := bob.Fingerprint()
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	st := alice.Groups.Get(gid)
	sigBefore := alice.Friends.Get(bobFp).Card.Sig

	mallory, err := a2a.NewIdentity(t.TempDir(), "mallory", []string{relayURL})
	if err != nil {
		t.Fatal(err)
	}
	mcard, _ := mallory.Card()
	bcard, _ := bob.Card()

	// (a) somebody else's card claimed as bob's.
	if alice.absorbGroupCard(ctx, st, bobFp, mcard) {
		t.Fatal("a card whose fingerprint is not the sender's must be ignored")
	}
	// (b) bob's card with the name changed but not re-signed.
	tampered := *bcard
	tampered.Name = "Evil"
	if alice.absorbGroupCard(ctx, st, bobFp, &tampered) {
		t.Fatal("a card that does not verify must be ignored")
	}
	// (c) a valid card from someone who is not on the roster.
	if alice.absorbGroupCard(ctx, st, mallory.Fingerprint(), mcard) {
		t.Fatal("a non-member's card must be ignored")
	}
	if got := alice.Groups.Get(gid).Roster.Version; got != v0 {
		t.Fatalf("rejected cards moved the roster to v%d", got)
	}
	if len(alice.Groups.MemberCards(gid)) != 0 || alice.Friends.Get(bobFp).Card.Sig != sigBefore {
		t.Fatal("rejected cards must leave the cache and the friend record alone")
	}

	// (d) bob's genuine re-signed card: friend refreshed, roster converged (v+1).
	renamed := *bob.Identity()
	renamed.Name = "Bobby"
	newCard, _ := renamed.Card()
	if !alice.absorbGroupCard(ctx, st, bobFp, newCard) {
		t.Fatal("a genuine renamed card must be absorbed")
	}
	st = alice.Groups.Get(gid)
	if st.Roster.Version != v0+1 || st.Roster.Member(bobFp).Name != "Bobby" {
		t.Fatalf("owner did not converge the roster: v%d member=%q", st.Roster.Version, st.Roster.Member(bobFp).Name)
	}
	if fr := alice.Friends.Get(bobFp); fr.Note != "Bobby" || fr.Card.Sig != newCard.Sig {
		t.Fatalf("friend record not refreshed: note=%q", fr.Note)
	}
	if len(alice.Groups.MemberCards(gid)) != 0 {
		t.Fatal("once the roster carries the card the owner keeps no announced copy")
	}
	// (e) the same card again: nothing to do, no second republish.
	if alice.absorbGroupCard(ctx, st, bobFp, newCard) {
		t.Fatal("the same card version must not be absorbed twice")
	}
	if got := alice.Groups.Get(gid).Roster.Version; got != v0+1 {
		t.Fatalf("roster republished twice for one card version: v%d", got)
	}
}

// TestRefreshFriendCardNoteRule: the friend note follows a rename only while it was never
// hand-edited; a typed note stays while the card snapshot still refreshes.
func TestRefreshFriendCardNoteRule(t *testing.T) {
	n, err := Init(t.TempDir(), "http://relay.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.EnsureIdentity("me"); err != nil {
		t.Fatal(err)
	}
	other, err := a2a.NewIdentity(t.TempDir(), "Ann", []string{"http://relay.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	fp := other.Fingerprint()
	c1, _ := other.Card()
	if err := n.Friends.Add(c1, ""); err != nil { // note defaults to the nickname
		t.Fatal(err)
	}

	// Not a friend / nil card: no-op, no error.
	if ok, err := n.RefreshFriendCard("nobody", c1); ok || err != nil {
		t.Fatalf("non-friend: ok=%v err=%v", ok, err)
	}
	if ok, err := n.RefreshFriendCard(fp, nil); ok || err != nil {
		t.Fatalf("nil card: ok=%v err=%v", ok, err)
	}
	// Same card: nothing changes.
	if ok, err := n.RefreshFriendCard(fp, c1); ok || err != nil {
		t.Fatalf("same card: ok=%v err=%v", ok, err)
	}

	// Rename with the note untouched: note follows.
	other.Name = "Annie"
	c2, _ := other.Card()
	if ok, err := n.RefreshFriendCard(fp, c2); !ok || err != nil {
		t.Fatalf("rename #1: ok=%v err=%v", ok, err)
	}
	if fr := n.Friends.Get(fp); fr.Note != "Annie" || fr.Card.Sig != c2.Sig {
		t.Fatalf("note should follow the nickname: note=%q", fr.Note)
	}

	// Hand-edited note: stays, card still refreshes.
	if err := n.Friends.SetNote(fp, "Ann from work"); err != nil {
		t.Fatal(err)
	}
	other.Name = "Anne"
	c3, _ := other.Card()
	if ok, err := n.RefreshFriendCard(fp, c3); !ok || err != nil {
		t.Fatalf("rename #2: ok=%v err=%v", ok, err)
	}
	if fr := n.Friends.Get(fp); fr.Note != "Ann from work" || fr.Card.Name != "Anne" {
		t.Fatalf("hand-edited note must stay: note=%q card=%q", fr.Note, fr.Card.Name)
	}

	// Somebody else's card under this friend's fingerprint: refused.
	stranger, _ := a2a.NewIdentity(t.TempDir(), "X", []string{"http://relay.invalid"})
	sc, _ := stranger.Card()
	if ok, err := n.RefreshFriendCard(fp, sc); ok || err == nil {
		t.Fatalf("foreign card: ok=%v err=%v", ok, err)
	}
	// Tampered card: refused.
	bad := *c3
	bad.Name = "Mallory"
	if ok, err := n.RefreshFriendCard(fp, &bad); ok || err == nil {
		t.Fatalf("tampered card: ok=%v err=%v", ok, err)
	}
	if fr := n.Friends.Get(fp); fr.Card.Sig != c3.Sig {
		t.Fatal("refused cards must not touch the snapshot")
	}
}
