package peer

import (
	"context"
	"testing"
)

// A friend that has dropped us (here: bob removed alice) used to discard alice's mail
// silently while alice still showed bob as a friend. Now bob bounces a not_friend and
// alice gets EventNotFriend naming the dropped message; once they are friends again the
// mail goes through and no bounce comes back.
func TestNotFriendBounce(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)

	if err := b.Friends.Remove(a.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	res, err := a.Send(context.Background(), b.Fingerprint(), "still there?", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev := a.await(t, "friend.not_mutual", kindIs(EventNotFriend))
	if ev.Peer != b.Fingerprint() || ev.Message == nil || ev.Message.Body != res.ID {
		t.Fatalf("bounce should name bob and the dropped message %s: %+v", res.ID, ev)
	}
	if got := b.Conversation(a.Fingerprint(), 0, 0); len(got) != 0 {
		t.Fatalf("bob must not archive mail from a non-friend: %+v", got)
	}

	// A stranger's bounce is ignored: alice drops bob too, bob's bounce (if any) must not
	// surface. Re-befriend and the next message is delivered normally.
	if err := a.Friends.Remove(b.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	befriend(t, a, b)
	if _, err := a.Send(context.Background(), b.Fingerprint(), "back again", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	b.await(t, "message.received", kindIs(EventMessageReceived))
}

// The case that hit a real group: the owner still holds bob as a friend, bob does not hold
// the owner, so bob silently dropped the group invite. Now the bounce carries the group id.
func TestNotFriendBounceGroupInvite(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)
	if err := b.Friends.Remove(a.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	view, err := a.GroupCreate(context.Background(), "docs", []string{b.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	ev := a.await(t, "friend.not_mutual for the invite", func(e Event) bool { return e.Kind == EventNotFriend && e.GID != "" })
	if ev.Peer != b.Fingerprint() || ev.GID != view.GID {
		t.Fatalf("bounce should name bob and group %s: %+v", view.GID, ev)
	}
	if b.Groups.Get(view.GID) != nil {
		t.Fatal("bob must not have joined")
	}
}

// After bob re-adds alice, the owner resends the invite and bob actually joins.
func TestGroupResendInviteAfterReadd(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)
	if err := b.Friends.Remove(a.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	view, err := a.GroupCreate(context.Background(), "docs", []string{b.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	a.await(t, "invite bounce", func(e Event) bool { return e.Kind == EventNotFriend && e.GID == view.GID })
	befriend(t, a, b)
	if err := a.GroupResendInvite(context.Background(), view.GID, b.Fingerprint()); err != nil {
		t.Fatalf("GroupResendInvite: %v", err)
	}
	b.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == view.GID })
	if b.Groups.Get(view.GID) == nil {
		t.Fatal("bob should have joined after the resent invite")
	}
	if err := b.GroupResendInvite(context.Background(), view.GID, a.Fingerprint()); err == nil {
		t.Fatal("a plain member must not be able to resend invites")
	}
}
