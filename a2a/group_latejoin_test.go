package a2a

import "testing"

// A LATE JOINER receives the sender's CURRENT ratcheted chain (the sender has
// already posted messages), not the epoch's origin. The key handout must carry
// the chain's position: stored at its true index the next message opens; stored
// at index 0 (the old bug) the ratchet desynchronizes and decryption fails.
func TestLateJoinerReceivesMidChainKey(t *testing.T) {
	sk, err := NewGroupSenderKey(1)
	if err != nil {
		t.Fatal(err)
	}
	// The sender talks for a while before the newcomer exists.
	for i := 0; i < 5; i++ {
		if _, err := GroupSeal(sk, &Message{ID: "warm", Type: TypeText, Body: "old talk"}); err != nil {
			t.Fatal(err)
		}
	}
	// Handout snapshot exactly as distributeSenderKey builds it.
	dist := GroupKeyDist{Epoch: sk.Epoch, Index: sk.Index, Chain: sk.Chain}

	cipher, err := GroupSeal(sk, &Message{ID: "m6", Type: TypeText, Body: "hello newcomer"})
	if err != nil {
		t.Fatal(err)
	}

	// Correct storage: chain at its declared position.
	good := &GroupRecvState{Epoch: dist.Epoch, Index: dist.Index, Chain: dist.Chain}
	msg, err := GroupOpen(good, cipher)
	if err != nil {
		t.Fatalf("late joiner with indexed key must decrypt: %v", err)
	}
	if msg.Body != "hello newcomer" {
		t.Fatalf("body = %q", msg.Body)
	}

	// The old bug: same chain stored at index 0 must NOT decrypt (proves the
	// index is load-bearing, not decorative).
	bad := &GroupRecvState{Epoch: dist.Epoch, Index: 0, Chain: dist.Chain}
	if _, err := GroupOpen(bad, cipher); err == nil {
		t.Fatal("mid-chain key stored at index 0 decrypted - the regression guard is meaningless")
	}
}
