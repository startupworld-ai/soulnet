package peer

import (
	"context"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupKeyRequestHeals reproduces "the receiver's keys.json was replaced by an older
// copy" (another device's data imported over it): the receive state for a co-member is
// gone, or is from an epoch the member has since left. The sender's distribution record
// still says "delivered", so without group_key_req nothing ever reaches the receiver
// again. With it, the receiver asks once it fails to open a message and everything sent
// afterwards opens.
func TestGroupKeyRequestHeals(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "lost keys", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	aliceFp, bobFp := alice.Fingerprint(), bob.Fingerprint()

	send := func(body string) {
		t.Helper()
		if res, err := alice.GroupSend(ctx, gid, body, GroupSendOptions{}); err != nil || res.Status != "sent" {
			t.Fatalf("GroupSend %q: %+v %v", body, res, err)
		}
	}
	bobHas := func(body string, wait time.Duration) bool {
		deadline := time.Now().Add(wait)
		for {
			for _, e := range bob.GroupConversation(gid, 0, 0) {
				if e.Body == body {
					return true
				}
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	bobHolds := func(epoch int) func() bool {
		return func() bool {
			rs := bob.Groups.Keys(gid).Senders[aliceFp]
			return rs != nil && rs.Epoch == epoch
		}
	}
	// Each case stands for a separate incident; the pacing that spaces real requests out
	// would otherwise hold the second one back.
	resetPacing := func() {
		for _, n := range []*testNode{alice, bob} {
			n.kqMu.Lock()
			n.kqSeen = map[string]time.Time{}
			n.kqMu.Unlock()
		}
	}

	send("one")
	if !bobHas("one", 30*time.Second) {
		t.Fatal("bob never got the setup message")
	}
	staleBob := bob.Groups.Keys(gid) // what an older copy of bob's keys.json looks like later on
	epoch1 := alice.Groups.Keys(gid).Mine.Epoch
	if got := alice.Groups.Keys(gid).DistributedTo[bobFp]; got != epoch1 {
		t.Fatalf("setup: alice records bob at epoch %d, want %d", got, epoch1)
	}

	// Case 1: the receive state is gone altogether (KEY-MISSING).
	keys := bob.Groups.Keys(gid)
	delete(keys.Senders, aliceFp)
	if err := bob.Groups.PutKeys(gid, keys); err != nil {
		t.Fatal(err)
	}
	send("lost while the key was missing")
	waitUntil(t, "bob holds alice's chain again after asking", bobHolds(epoch1))
	send("after the first recovery")
	if !bobHas("after the first recovery", 30*time.Second) {
		t.Fatal("message after the key came back was not received")
	}

	// Case 2: alice moved to a new epoch (device switch), bob got it - and then bob's
	// keys.json went back to the copy from before.
	resetPacing()
	if _, err := alice.RotateSenderKeys("test: device switch"); err != nil {
		t.Fatal(err)
	}
	epoch2 := alice.Groups.Keys(gid).Mine.Epoch
	send("on the new epoch")
	if !bobHas("on the new epoch", 30*time.Second) {
		t.Fatal("bob never got the first message of the new epoch")
	}
	now := bob.Groups.Keys(gid)
	now.Senders[aliceFp] = staleBob.Senders[aliceFp]
	if err := bob.Groups.PutKeys(gid, now); err != nil {
		t.Fatal(err)
	}
	send("lost while bob held the old epoch")
	waitUntil(t, "bob holds alice's new epoch again after asking", bobHolds(epoch2))
	send("after the second recovery")
	if !bobHas("after the second recovery", 30*time.Second) {
		t.Fatal("message after the epoch came back was not received")
	}
}

// TestGroupKeyRequestOnlyServesMembers: a request from someone who is not on the roster
// (never was, or was removed) must not make me hand anything out or touch my records.
func TestGroupKeyRequestOnlyServesMembers(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "members only", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	if res, err := alice.GroupSend(ctx, gid, "hello", GroupSendOptions{}); err != nil || res.Status != "sent" {
		t.Fatalf("GroupSend: %+v %v", res, err)
	}
	before := alice.Groups.Keys(gid)

	req := &a2a.Message{ID: "req-1", From: carol.Fingerprint(), To: alice.Fingerprint(), TS: time.Now(), Type: a2a.TypeGroupKeyReq, GID: gid}
	if handled, err := alice.HandleGroupMessage(req); !handled || err != nil {
		t.Fatalf("group_key_req from a non-member: handled=%v err=%v, want handled and no error", handled, err)
	}
	after := alice.Groups.Keys(gid)
	if after.DistributedTo[bob.Fingerprint()] != before.DistributedTo[bob.Fingerprint()] || after.Mine.Epoch != before.Mine.Epoch || after.Mine.Index != before.Mine.Index {
		t.Fatalf("a non-member's request changed my key state: %+v -> %+v", before, after)
	}
	if _, ok := after.DistributedTo[carol.Fingerprint()]; ok {
		t.Fatal("a non-member ended up in my distribution record")
	}

	// A malformed request is a permanent error (ack and drop), not a retry loop.
	bad := &a2a.Message{ID: "req-2", From: bob.Fingerprint(), To: alice.Fingerprint(), TS: time.Now(), Type: a2a.TypeGroupKeyReq, GID: "not a group id"}
	if _, err := alice.HandleGroupMessage(bad); err == nil || !IsPermanent(err) {
		t.Fatalf("malformed group_key_req: err=%v, want a permanent error", err)
	}
}

// TestKeyReqPacing: the same paced action runs once per interval; different keys do not
// hold each other back.
func TestKeyReqPacing(t *testing.T) {
	n := &Peer{kqSeen: map[string]time.Time{}}
	if !n.keyReqDue("ask|g|a", time.Hour) {
		t.Fatal("first call must be due")
	}
	if n.keyReqDue("ask|g|a", time.Hour) {
		t.Fatal("second call inside the interval must be held back")
	}
	if !n.keyReqDue("ask|g|b", time.Hour) {
		t.Fatal("another key must not be held back")
	}
	if !n.keyReqDue("ask|g|a", 0) {
		t.Fatal("an elapsed interval must be due again")
	}
}
