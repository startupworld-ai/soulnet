package a2a

import "testing"

// The consumed-tx ledger is the replay guard for paid groups: one on-chain
// payment admits exactly one member. ConsumePaymentTx must be atomic (first
// claim wins), idempotent across instances (file-backed), and scoped per group.
func TestGroupStore_ConsumePaymentTx(t *testing.T) {
	dir := t.TempDir()
	st := NewGroupStore(dir)
	gid := "g1"
	tx := "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7"

	// First claim wins.
	ok, err := st.ConsumePaymentTx(gid, tx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("first claim should succeed")
	}
	// Reuse is refused — the replay attack.
	ok, err = st.ConsumePaymentTx(gid, tx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second claim of the same tx must fail (replay)")
	}

	// A different group can claim its own tx independently.
	if ok, err := st.ConsumePaymentTx("g2", tx); err != nil || !ok {
		t.Fatalf("g2 claim: ok=%v err=%v", ok, err)
	}

	// A fresh store instance (simulated restart) remembers the claim.
	st2 := NewGroupStore(dir)
	if ok, err := st2.ConsumePaymentTx(gid, tx); err != nil || ok {
		t.Fatalf("claim after restart must be refused (persistence): ok=%v err=%v", ok, err)
	}
	if ok, err := st2.ConsumePaymentTx(gid, "0x"+"00"+tx[2:]); err != nil || !ok {
		t.Fatalf("a different tx must still be claimable: ok=%v err=%v", ok, err)
	}
}
