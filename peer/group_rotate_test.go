package peer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestGroupRotateAfterStaleKeys reproduces "the same identity continues on another device
// with an older copy of keys.json": the sender's chain goes back to a position the members
// already consumed, and every message from there is dropped as a replay. RotateSenderKeys
// (what a host calls after such a device switch) moves to a fresh epoch - without the
// explicit distribution too, because the next send tops it up before sealing.
func TestGroupRotateAfterStaleKeys(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "rewind", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

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

	send("one")
	stale := alice.Groups.Keys(gid) // the copy another device took along at this point
	send("two")
	send("three")
	if !bobHas("three", 30*time.Second) {
		t.Fatal("bob never got the setup messages")
	}

	// The stale copy lands back (device switch): the next message reuses index 1.
	if err := alice.Groups.PutKeys(gid, stale); err != nil {
		t.Fatal(err)
	}
	send("rewound")
	if bobHas("rewound", 2*time.Second) {
		t.Fatal("the rewound message was accepted - the reproduction no longer holds")
	}

	// SenderKeyMarks sees the rewrite (what a host compares around an import).
	marks := alice.SenderKeyMarks()
	if m := marks[gid]; m == "" || !strings.HasPrefix(m, "1/2/") {
		t.Fatalf("SenderKeyMarks after the rewound send: %q, want epoch 1 index 2", m)
	}

	// Fixed path: rotate (local only), then send - the first send distributes the new chain.
	before := alice.Groups.Keys(gid).Mine.Epoch
	rotated, err := alice.RotateSenderKeys("test: device switch")
	if err != nil || len(rotated) != 1 || rotated[0] != gid {
		t.Fatalf("RotateSenderKeys: %v %v", rotated, err)
	}
	mine := alice.Groups.Keys(gid).Mine
	if mine.Epoch <= before || mine.Index != 0 || mine.Epoch < int(time.Now().Add(-time.Minute).Unix()) {
		t.Fatalf("rotation: epoch %d -> %+v, want a fresh chain at a clock-floored epoch", before, mine)
	}
	send("after rotation 1")
	send("after rotation 2")
	if !bobHas("after rotation 1", 30*time.Second) || !bobHas("after rotation 2", 30*time.Second) {
		t.Fatal("messages after the rotation were not received")
	}
	if rs := bob.Groups.Keys(gid).Senders[alice.Fingerprint()]; rs == nil || rs.Epoch != mine.Epoch {
		t.Fatalf("bob holds alice at %+v, want epoch %d", rs, mine.Epoch)
	}

	// A plain second rotation stays monotonic, and the explicit distribution reaches bob
	// before anything is sent.
	if _, err := alice.RotateSenderKeys("again"); err != nil {
		t.Fatal(err)
	}
	again := alice.Groups.Keys(gid).Mine.Epoch
	if again <= mine.Epoch {
		t.Fatalf("second rotation went from %d to %d", mine.Epoch, again)
	}
	alice.DistributeSenderKeys(ctx)
	waitUntil(t, "bob holds the second rotation", func() bool {
		rs := bob.Groups.Keys(gid).Senders[alice.Fingerprint()]
		return rs != nil && rs.Epoch == again
	})
	send("after rotation 3")
	if !bobHas("after rotation 3", 30*time.Second) {
		t.Fatal("message after the second rotation was not received")
	}
}

// TestRotateSkipsLeftGroups: a group I was removed from stays read-only - no rotation.
func TestRotateSkipsLeftGroups(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "kick", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	if err := alice.GroupKick(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "bob is marked as removed", func() bool { return bob.GroupLeft(gid) })
	before := bob.Groups.Keys(gid).Mine
	rotated, err := bob.RotateSenderKeys("test")
	if err != nil || len(rotated) != 0 {
		t.Fatalf("rotated %v (%v), want none", rotated, err)
	}
	if after := bob.Groups.Keys(gid).Mine; before == nil || after == nil || after.Epoch != before.Epoch || after.Chain != before.Chain {
		t.Fatalf("left group's key changed: %+v -> %+v", before, after)
	}
}

func TestNextSenderEpoch(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	for _, c := range []struct{ cur, want int }{
		{0, 1_790_000_000},
		{3, 1_790_000_000},
		{1_790_000_000, 1_790_000_001}, // a rotation within the same second stays monotonic
		{1_900_000_000, 1_900_000_001}, // a clock behind the last rotation never goes back
	} {
		if got := a2a.NextSenderEpoch(c.cur, now); got != c.want {
			t.Errorf("NextSenderEpoch(%d) = %d, want %d", c.cur, got, c.want)
		}
	}
}
