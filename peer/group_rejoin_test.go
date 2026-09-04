package peer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// eventLog replaces a test node's event channel with an unbounded, lockable record so a
// long scenario never blocks the receive loop on a full channel and assertions can look
// back at every event (kick → re-admit produces dozens of group.updated).
type eventLog struct {
	mu  sync.Mutex
	evs []Event
}

func recordEvents(tn *testNode) *eventLog {
	l := &eventLog{}
	tn.OnEvent = func(ev Event) {
		l.mu.Lock()
		l.evs = append(l.evs, ev)
		l.mu.Unlock()
	}
	return l
}

func (l *eventLog) has(pred func(Event) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ev := range l.evs {
		if pred(ev) {
			return true
		}
	}
	return false
}

// groupHas reports whether the node archived a group entry with this body.
func groupHas(n *Peer, gid, body string) bool {
	for _, e := range n.GroupConversation(gid, 0, 0) {
		if e.Body == body {
			return true
		}
	}
	return false
}

// sendAndExpect sends body from one member and waits until every other listed member
// has decrypted and archived it.
func sendAndExpect(t *testing.T, gid, body string, from *testNode, to ...*testNode) {
	t.Helper()
	res, err := from.GroupSend(context.Background(), gid, body, GroupSendOptions{})
	if err != nil || res.Status != "sent" {
		t.Fatalf("GroupSend(%q): %+v %v", body, res, err)
	}
	for _, r := range to {
		r := r
		waitUntil(t, r.Identity().Name+" decrypts "+body, func() bool { return groupHas(r.Peer, gid, body) })
	}
}

// threeWay has every member post once and everyone else decrypt it.
func threeWay(t *testing.T, gid, tag string, nodes ...*testNode) {
	t.Helper()
	for i, from := range nodes {
		var others []*testNode
		for j, n := range nodes {
			if j != i {
				others = append(others, n)
			}
		}
		sendAndExpect(t, gid, tag+"-"+from.Identity().Name, from, others...)
	}
}

// stopLoop / startLoop take a node offline and back (same Peer, same home): the relay
// keeps its mailbox meanwhile.
func stopLoop(t *testing.T, tn *testNode) {
	t.Helper()
	tn.cancel()
	select {
	case <-tn.done:
	case <-time.After(10 * time.Second):
		t.Fatal("receive loop did not stop")
	}
}

func startLoop(t *testing.T, tn *testNode) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	tn.cancel, tn.done = cancel, done
	go func() {
		defer close(done)
		_ = tn.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
}

// TestGroupKickRejoinRealignsKeys: three members chat → the owner removes one → the
// removed node keeps the group read-only (history intact, sending refused, no new mail)
// while the remaining two keep chatting → the owner re-admits it → all three decrypt
// each other again, including two consecutive posts from the returning member (which
// must not be taken for replays of its pre-removal chain).
func TestGroupKickRejoinRealignsKeys(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	recordEvents(alice)
	bLog, cLog := recordEvents(bob), recordEvents(carol)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "rejoin club", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	joined := func(l *eventLog) func() bool {
		return func() bool {
			return l.has(func(e Event) bool {
				return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonJoined
			})
		}
	}
	waitUntil(t, "bob joins (group.updated/joined)", joined(bLog))
	waitUntil(t, "carol joins (group.updated/joined)", joined(cLog))
	threeWay(t, gid, "round1", alice, bob, carol)

	// Owner removes bob.
	if err := alice.GroupKick(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatalf("GroupKick: %v", err)
	}
	waitUntil(t, "bob marked as removed", func() bool { return bob.GroupLeft(gid) })
	// The group stays on disk. Which roster it holds depends on how the removal reached
	// bob first: the owner's pairwise poke makes him refetch and the relay answers 403
	// (roster stays at v1), a fan-out he could still decrypt would have carried v2.
	if st := bob.Groups.Get(gid); st == nil || st.Roster.Version < 1 {
		t.Fatalf("removed member must keep the group: %+v", st)
	}
	if !groupHas(bob.Peer, gid, "round1-alice") || !groupHas(bob.Peer, gid, "round1-bob") {
		t.Fatal("removed member must keep its history")
	}
	if list := bob.GroupList(); len(list) != 1 || !list[0].Left {
		t.Fatalf("removed member's list row must carry left=true: %+v", list)
	}
	if _, err := bob.GroupSend(ctx, gid, "still here?", GroupSendOptions{}); !errors.Is(err, ErrGroupLeft) {
		t.Fatalf("GroupSend after removal: want ErrGroupLeft, got %v", err)
	}
	if !bLog.has(func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonRemoved
	}) {
		t.Fatal("bob should have seen group.updated/removed")
	}
	if !cLog.has(func(e Event) bool {
		return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonRoster && len(e.Removed) == 1 && e.Removed[0] == bob.Fingerprint()
	}) {
		t.Fatal("carol should have seen group.updated/roster with bob in Removed")
	}
	// The remaining two rotated and keep talking; bob gets nothing new.
	sendAndExpect(t, gid, "after-kick-alice", alice, carol)
	sendAndExpect(t, gid, "after-kick-carol", carol, alice)
	time.Sleep(1500 * time.Millisecond)
	if groupHas(bob.Peer, gid, "after-kick-alice") || groupHas(bob.Peer, gid, "after-kick-carol") {
		t.Fatal("removed member must not receive new mail")
	}
	if k := alice.Groups.Keys(gid); k.Mine.Epoch != 2 || k.Senders[bob.Fingerprint()] != nil || k.DistributedTo[bob.Fingerprint()] != 0 {
		t.Fatalf("owner after kick: want epoch 2 and bob forgotten, got mine=%+v senders=%v dist=%v", k.Mine, k.Senders, k.DistributedTo)
	}

	// Owner re-admits bob: same conversation, keys realign.
	if err := alice.GroupInvite(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatalf("GroupInvite: %v", err)
	}
	waitUntil(t, "bob re-admitted (group.updated/rejoined)", func() bool {
		return bLog.has(func(e Event) bool {
			return e.Kind == EventGroupUpdated && e.GID == gid && e.Reason == GroupReasonRejoined
		})
	})
	if bob.GroupLeft(gid) || bob.Groups.Get(gid).Roster.Version != 3 {
		t.Fatal("re-admitted member must hold the new roster without the removed marker")
	}
	if list := bob.GroupList(); len(list) != 1 || list[0].Left {
		t.Fatalf("re-admitted member's list row must not carry left: %+v", list)
	}
	threeWay(t, gid, "round2", alice, bob, carol)
	sendAndExpect(t, gid, "bob-again-1", bob, alice, carol)
	sendAndExpect(t, gid, "bob-again-2", bob, alice, carol)
	if !groupHas(bob.Peer, gid, "round1-alice") || !groupHas(bob.Peer, gid, "round2-alice") {
		t.Fatal("re-admission must continue the same conversation")
	}
	// Epochs: everyone rotated exactly once (owner + carol on the removal, bob on the
	// re-admission) and holds the others at those epochs.
	for _, n := range []*testNode{alice, bob, carol} {
		k := n.Groups.Keys(gid)
		if k.Mine.Epoch != 2 {
			t.Fatalf("%s: want my epoch 2, got %d", n.Identity().Name, k.Mine.Epoch)
		}
		for _, other := range []*testNode{alice, bob, carol} {
			if other == n {
				continue
			}
			if rs := k.Senders[other.Fingerprint()]; rs == nil || rs.Epoch != 2 {
				t.Fatalf("%s holds %s at %+v, want epoch 2", n.Identity().Name, other.Identity().Name, rs)
			}
		}
	}
}

// TestGroupLateJoinerDecryptsImmediately: the owner's chain has advanced when a new
// member is added; the distribution must carry the chain position so the newcomer
// decrypts the very next message (storing it at index 0 would fail every message).
func TestGroupLateJoinerDecryptsImmediately(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	recordEvents(alice)
	recordEvents(bob)
	recordEvents(carol)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "late club", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	waitUntil(t, "bob joins", func() bool { return bob.Groups.Get(gid) != nil })
	for i := 0; i < 3; i++ {
		sendAndExpect(t, gid, "early-"+string(rune('a'+i)), alice, bob)
	}
	if k := alice.Groups.Keys(gid); k.Mine.Index < 3 {
		t.Fatalf("owner's chain should have advanced, index=%d", k.Mine.Index)
	}
	if err := alice.GroupInvite(ctx, gid, carol.Fingerprint()); err != nil {
		t.Fatalf("GroupInvite: %v", err)
	}
	waitUntil(t, "carol joins", func() bool { return carol.Groups.Get(gid) != nil })
	sendAndExpect(t, gid, "late-hello", alice, bob, carol)
	if rs := carol.Groups.Keys(gid).Senders[alice.Fingerprint()]; rs == nil || rs.Index < 4 {
		t.Fatalf("carol must hold alice's chain at its real position, got %+v", rs)
	}
	sendAndExpect(t, gid, "from-bob", bob, alice, carol)
	sendAndExpect(t, gid, "from-carol", carol, alice, bob)
}

// TestGroupRejoinAfterOfflineRemoval: a member is removed AND re-added while its node is
// offline, so it never sees a roster in which it is missing (no removal marker, no rekey
// on its side). The invite for a group it already holds must still make it hand its key
// to everyone again - the others forgot it when they rotated.
func TestGroupRejoinAfterOfflineRemoval(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	recordEvents(alice)
	recordEvents(bob)
	recordEvents(carol)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "offline club", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	waitUntil(t, "bob joins", func() bool { return bob.Groups.Get(gid) != nil })
	waitUntil(t, "carol joins", func() bool { return carol.Groups.Get(gid) != nil })
	threeWay(t, gid, "round1", alice, bob, carol)

	stopLoop(t, bob)
	if err := alice.GroupKick(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatalf("GroupKick: %v", err)
	}
	sendAndExpect(t, gid, "while-out", alice, carol)
	if err := alice.GroupInvite(ctx, gid, bob.Fingerprint()); err != nil {
		t.Fatalf("GroupInvite: %v", err)
	}
	waitUntil(t, "carol sees bob back", func() bool { return carol.Groups.Get(gid).Roster.Version == 3 })
	sendAndExpect(t, gid, "welcome-back", alice, carol)
	startLoop(t, bob)

	waitUntil(t, "bob decrypts the post-re-admission message", func() bool { return groupHas(bob.Peer, gid, "welcome-back") })
	if groupHas(bob.Peer, gid, "while-out") {
		t.Fatal("a removed member must not obtain mail sent while it was out")
	}
	if bob.GroupLeft(gid) {
		t.Fatal("bob never saw the removal and must not be marked as removed")
	}
	// The crux: alice and carol forgot bob's key on the removal; bob, having seen no
	// removal, did not rotate - the invite must have made it redistribute.
	sendAndExpect(t, gid, "bob-is-back", bob, alice, carol)
	sendAndExpect(t, gid, "bob-is-back-2", bob, alice, carol)
	threeWay(t, gid, "round2", alice, bob, carol)
}

// ——— unit level: the key-handling rules, no relay ———

type unitGroup struct {
	n     *Peer
	gid   string
	st    *a2a.GroupState
	other map[string]*a2a.Identity // name → identity of a co-member
}

// newUnitGroup builds a peer that owns a group with the named co-members. Member cards
// point at a closed port so any network send fails fast (and is queued).
func newUnitGroup(t *testing.T, members ...string) *unitGroup {
	t.Helper()
	dead := "http://127.0.0.1:1"
	n, err := Init(filepath.Join(t.TempDir(), "home"), dead)
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(string, ...any) {}
	id, err := n.EnsureIdentity("me")
	if err != nil {
		t.Fatal(err)
	}
	myCard, _ := id.Card()
	ug := &unitGroup{n: n, other: map[string]*a2a.Identity{}}
	cards := []*a2a.Card{myCard}
	for _, name := range members {
		oid, err := a2a.NewIdentity(t.TempDir(), name, []string{dead})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := oid.Card()
		cards = append(cards, c)
		ug.other[name] = oid
	}
	gid, _ := a2a.NewGroupID()
	roster := &a2a.GroupRoster{V: 1, GroupID: gid, Name: "unit", OwnerPub: id.EdPub, Relay: dead,
		Version: 1, Members: cards, TS: time.Now(), Profile: a2a.DefaultGroupProfile()}
	priv, _ := id.EdPrivate()
	roster.Sign(priv)
	if err := roster.Verify(); err != nil {
		t.Fatal(err)
	}
	ug.gid = gid
	ug.st = &a2a.GroupState{Roster: roster, JoinedAt: time.Now()}
	if err := n.Groups.Put(ug.st); err != nil {
		t.Fatal(err)
	}
	return ug
}

func (ug *unitGroup) keyMsg(from string, epoch, index int, chain string) *a2a.Message {
	return &a2a.Message{ID: from + "-" + chain[:6], From: from, To: ug.n.Fingerprint(), TS: time.Now(),
		Type: a2a.TypeGroupKey, GID: ug.gid, GKey: &a2a.GroupKeyDist{Epoch: epoch, Index: index, Chain: chain}}
}

func TestHandleGroupKeyStoresPositionAndRestarts(t *testing.T) {
	ug := newUnitGroup(t, "x")
	x := ug.other["x"].Fingerprint()
	n := ug.n

	// x sealed three messages before distributing: the chain arrives at index 3 and the
	// fourth message must open with what we store.
	sk, _ := a2a.NewGroupSenderKey(1)
	for i := 0; i < 3; i++ {
		if _, err := a2a.GroupSeal(sk, &a2a.Message{ID: "m", From: x, GID: ug.gid, Type: a2a.TypeText, Body: "early"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.handleGroupKey(ug.keyMsg(x, sk.Epoch, sk.Index, sk.Chain)); err != nil {
		t.Fatalf("first distribution: %v", err)
	}
	rs := n.Groups.Keys(ug.gid).Senders[x]
	if rs == nil || rs.Epoch != 1 || rs.Index != 3 {
		t.Fatalf("want {epoch 1, index 3}, got %+v", rs)
	}
	chainAt3 := sk.Chain
	fourth, _ := a2a.GroupSeal(sk, &a2a.Message{ID: "m4", From: x, GID: ug.gid, Type: a2a.TypeText, Body: "fourth"})
	if m, err := a2a.GroupOpen(rs, fourth); err != nil || m.Body != "fourth" {
		t.Fatalf("message after a positioned distribution must open: %v", err)
	}
	// The same chain stored at index 0 would NOT open it (the bug this pins).
	if _, err := a2a.GroupOpen(&a2a.GroupRecvState{Epoch: 1, Index: 0, Chain: rs.Chain}, fourth); err == nil {
		t.Fatal("a chain stored at index 0 must not decrypt a message at index 3")
	}

	// Byte-identical redelivery: no change.
	held := *n.Groups.Keys(ug.gid).Senders[x]
	if err := n.handleGroupKey(ug.keyMsg(x, 1, 3, chainAt3)); err != nil {
		t.Fatal(err)
	}
	if got := *n.Groups.Keys(ug.gid).Senders[x]; got.Index != held.Index || got.Chain != held.Chain {
		t.Fatalf("redelivery must not change the held state: %+v vs %+v", got, held)
	}
	// Same epoch, different material (x restarted at epoch 1 / index 0): take it as stated.
	restart, _ := a2a.NewGroupSenderKey(1)
	if err := n.handleGroupKey(ug.keyMsg(x, 1, 0, restart.Chain)); err != nil {
		t.Fatal(err)
	}
	if rs = n.Groups.Keys(ug.gid).Senders[x]; rs.Epoch != 1 || rs.Index != 0 || rs.Chain != restart.Chain {
		t.Fatalf("a restarted chain at the same epoch must replace the held one, got %+v", rs)
	}
	// Higher epoch replaces; a lower one afterwards is ignored.
	e2, _ := a2a.NewGroupSenderKey(2)
	if err := n.handleGroupKey(ug.keyMsg(x, 2, 0, e2.Chain)); err != nil {
		t.Fatal(err)
	}
	if err := n.handleGroupKey(ug.keyMsg(x, 1, 5, restart.Chain)); err != nil {
		t.Fatal(err)
	}
	if rs = n.Groups.Keys(ug.gid).Senders[x]; rs.Epoch != 2 || rs.Chain != e2.Chain {
		t.Fatalf("a lower epoch must not replace a higher one, got %+v", rs)
	}
	// Malformed: negative index / epoch 0 are permanent.
	if err := n.handleGroupKey(ug.keyMsg(x, 1, -1, restart.Chain)); !isPermanent(err) {
		t.Fatalf("negative index must be permanent, got %v", err)
	}
	if err := n.handleGroupKey(ug.keyMsg(x, 0, 0, restart.Chain)); !isPermanent(err) {
		t.Fatalf("epoch 0 must be permanent, got %v", err)
	}
	// A sender that is not on my roster: transient (the roster may still be in flight),
	// not a poison letter.
	stranger, _ := a2a.NewIdentity(t.TempDir(), "z", []string{"http://127.0.0.1:1"})
	if err := n.handleGroupKey(ug.keyMsg(stranger.Fingerprint(), 1, 0, restart.Chain)); err == nil || isPermanent(err) {
		t.Fatalf("group_key from an unknown sender must be a transient error, got %v", err)
	}
}

func TestRekeyGroupForgetsNonMembers(t *testing.T) {
	ug := newUnitGroup(t, "x")
	x := ug.other["x"].Fingerprint()
	gone := "gone-fingerprint"
	n := ug.n
	if err := n.ensureSenderKey(ug.gid); err != nil {
		t.Fatal(err)
	}
	keys := n.Groups.Keys(ug.gid)
	keys.Senders[x] = &a2a.GroupRecvState{Epoch: 1, Index: 2, Chain: keys.Mine.Chain}
	keys.Senders[gone] = &a2a.GroupRecvState{Epoch: 1, Index: 7, Chain: keys.Mine.Chain}
	keys.DistributedTo[x] = 1
	keys.DistributedTo[gone] = 1
	if err := n.Groups.PutKeys(ug.gid, keys); err != nil {
		t.Fatal(err)
	}
	n.rekeyGroup(context.Background(), ug.st, "test")
	keys = n.Groups.Keys(ug.gid)
	if keys.Mine.Epoch != 2 || keys.Mine.Index != 0 {
		t.Fatalf("want a fresh epoch-2 chain, got %+v", keys.Mine)
	}
	if keys.Senders[gone] != nil || keys.Senders[x] == nil {
		t.Fatalf("rekey must forget non-members only: %v", keys.Senders)
	}
	if _, ok := keys.DistributedTo[gone]; ok {
		t.Fatalf("rekey must drop the distribution record of non-members: %v", keys.DistributedTo)
	}
}

func TestGroupLeftMarkerKeepsGroupReadOnly(t *testing.T) {
	ug := newUnitGroup(t, "x")
	n, gid := ug.n, ug.gid
	if err := n.Convs.Append(a2a.GroupConvKey(gid), &a2a.ConvEntry{Dir: "in", Message: a2a.Message{ID: "h1", From: "x", GID: gid, TS: time.Now(), Type: a2a.TypeText, Body: "history"}}); err != nil {
		t.Fatal(err)
	}
	if n.GroupLeft(gid) {
		t.Fatal("fresh group must not be marked")
	}
	n.markGroupLeft(gid, "removed")
	if !n.GroupLeft(gid) || n.Groups.Get(gid) == nil {
		t.Fatal("marker must be set and the group kept")
	}
	if list := n.GroupList(); len(list) != 1 || !list[0].Left {
		t.Fatalf("list row must carry left: %+v", list)
	}
	if len(n.GroupConversation(gid, 0, 0)) != 1 {
		t.Fatal("history must be kept")
	}
	if _, err := n.GroupSend(context.Background(), gid, "hello?", GroupSendOptions{}); !errors.Is(err, ErrGroupLeft) {
		t.Fatalf("want ErrGroupLeft, got %v", err)
	}
	if err := n.GroupAnnounceVoicesWith(context.Background(), gid, []string{"Bot"}, GroupVoicesOptions{}); !errors.Is(err, ErrGroupLeft) {
		t.Fatalf("fan-out must be refused while removed, got %v", err)
	}
	var raw map[string]any
	b, _ := os.ReadFile(n.leftPath(gid))
	if json.Unmarshal(b, &raw) != nil || raw["reason"] != "removed" {
		t.Fatalf("left.json must be {ts, reason}: %s", b)
	}
	n.markGroupLeft(gid, "removed") // idempotent
	n.clearGroupLeft(gid)
	if n.GroupLeft(gid) {
		t.Fatal("marker must clear")
	}
	n.markGroupLeft("00000000000000000000000000000000", "removed")
	if n.GroupLeft("00000000000000000000000000000000") {
		t.Fatal("a group that does not exist must not get a marker")
	}
}

func TestGroupLeaveOnRemovedGroupDeletesLocally(t *testing.T) {
	// A non-owner peer holding a group it was removed from: GroupLeave deletes the local
	// record without notifying anyone (the owner already knows).
	dead := "http://127.0.0.1:1"
	n, err := Init(filepath.Join(t.TempDir(), "home"), dead)
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(string, ...any) {}
	me, _ := n.EnsureIdentity("member")
	owner, _ := a2a.NewIdentity(t.TempDir(), "owner", []string{dead})
	oc, _ := owner.Card()
	mc, _ := me.Card()
	gid, _ := a2a.NewGroupID()
	roster := &a2a.GroupRoster{V: 1, GroupID: gid, Name: "g", OwnerPub: owner.EdPub, Relay: dead,
		Version: 1, Members: []*a2a.Card{oc, mc}, TS: time.Now()}
	priv, _ := owner.EdPrivate()
	roster.Sign(priv)
	if err := n.Groups.Put(&a2a.GroupState{Roster: roster, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	n.markGroupLeft(gid, "removed")
	if err := n.GroupLeave(context.Background(), gid); err != nil {
		t.Fatalf("GroupLeave on a removed group: %v", err)
	}
	if n.Groups.Get(gid) != nil || n.GroupLeft(gid) {
		t.Fatal("the local record must be gone")
	}
	if n.OutboxLen() != 0 {
		t.Fatal("no leave notice must be queued for a group I was removed from")
	}
}

func TestAppliedMarkerIsOwnerBoundAndExpires(t *testing.T) {
	ug := newUnitGroup(t)
	n := ug.n
	gid, _ := a2a.NewGroupID()
	n.markApplied(gid, "owner-A")
	if owner, ok := n.appliedOwner(gid); !ok || owner != "owner-A" || !n.GroupApplied(gid) {
		t.Fatalf("marker must record the owner: %q %v", owner, ok)
	}
	// Marker written before the owner was recorded: still whitelists (by gid only).
	legacy, _ := a2a.NewGroupID()
	_ = os.MkdirAll(filepath.Dir(n.appliedPath(legacy)), 0o755)
	_ = os.WriteFile(n.appliedPath(legacy), []byte(`{"ts":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`), 0o644)
	if owner, ok := n.appliedOwner(legacy); !ok || owner != "" {
		t.Fatalf("legacy marker must whitelist with an empty owner: %q %v", owner, ok)
	}
	// Expired: gone.
	old, _ := a2a.NewGroupID()
	_ = os.MkdirAll(filepath.Dir(n.appliedPath(old)), 0o755)
	stale := time.Now().Add(-appliedTTL - time.Hour).UTC().Format(time.RFC3339Nano)
	_ = os.WriteFile(n.appliedPath(old), []byte(`{"owner":"owner-B","ts":"`+stale+`"}`), 0o644)
	if _, ok := n.appliedOwner(old); ok || n.GroupApplied(old) {
		t.Fatal("expired marker must not whitelist")
	}
	if _, err := os.Stat(n.appliedPath(old)); !os.IsNotExist(err) {
		t.Fatal("expired marker must be removed")
	}
	n.clearApplied(gid)
	if n.GroupApplied(gid) {
		t.Fatal("cleared marker must not whitelist")
	}
}
