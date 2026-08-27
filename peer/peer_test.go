package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/relay"
)

// startRelay starts a real relay (httptest) and returns its base URL.
func startRelay(t *testing.T) string {
	t.Helper()
	srv, err := relay.New(t.TempDir())
	if err != nil {
		t.Fatalf("relay.New: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs.URL
}

type testNode struct {
	*Peer
	events chan Event
	cancel context.CancelFunc
	done   chan struct{}
}

// newTestNode creates an identity and starts the receive loop; events go to the events
// channel.
func newTestNode(t *testing.T, relayURL, name string) *testNode {
	t.Helper()
	n, err := Init(filepath.Join(t.TempDir(), "home"), relayURL)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	n.Logf = func(format string, args ...any) { t.Logf("["+name+"] "+format, args...) }
	if _, err := n.EnsureIdentity(name); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	tn := &testNode{Peer: n, events: make(chan Event, 64), done: make(chan struct{})}
	n.OnEvent = func(ev Event) { tn.events <- ev }
	ctx, cancel := context.WithCancel(context.Background())
	tn.cancel = cancel
	go func() {
		defer close(tn.done)
		_ = n.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tn.done:
		case <-time.After(5 * time.Second):
			t.Log("receive loop did not exit within 5s")
		}
	})
	return tn
}

// await waits for an event satisfying pred (others are discarded); fatal on timeout.
func (tn *testNode) await(t *testing.T, what string, pred func(Event) bool) Event {
	t.Helper()
	// 60s: the peer's transient-retry budget (retryBudget) is 60 attempts at
	// >=500ms apart (~30s) for in-flight races like a group sender key that has
	// not reached us yet. A 15s ceiling can fail on a busy runner before the
	// retry resolves, so the test timeout must comfortably exceed that budget.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev := <-tn.events:
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func kindIs(kind string) func(Event) bool { return func(e Event) bool { return e.Kind == kind } }

// befriend makes a request and b accept, so both become friends.
func befriend(t *testing.T, a, b *testNode) {
	t.Helper()
	bc, err := b.Card()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddFriend(context.Background(), bc.EncodeURI(), "hi, let's be friends"); err != nil {
		t.Fatalf("AddFriend: %v", err)
	}
	req := b.await(t, "friend.request", kindIs(EventFriendRequest))
	if req.Peer != a.Fingerprint() || req.PendingID == "" || req.Message == nil || req.Message.Body != "hi, let's be friends" {
		t.Fatalf("unexpected friend.request event: %+v", req)
	}
	if got := b.PendingRequests(); len(got) != 1 || got[0].ID != req.PendingID {
		t.Fatalf("unexpected pending list: %+v", got)
	}
	if _, err := b.Accept(context.Background(), req.PendingID, "friend A"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	acc := a.await(t, "friend.accepted", kindIs(EventFriendAccepted))
	if acc.Peer != b.Fingerprint() || acc.Friend == nil {
		t.Fatalf("unexpected friend.accepted event: %+v", acc)
	}
	if !a.Friends.IsFriend(b.Fingerprint()) || !b.Friends.IsFriend(a.Fingerprint()) {
		t.Fatal("both sides should now be friends")
	}
	if len(b.PendingRequests()) != 0 {
		t.Fatal("pending should be empty after accept")
	}
}

func TestHandshakeTextAndReadCursor(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")

	// file layout
	if st, err := os.Stat(filepath.Join(a.Home, "a2a", "identity.json")); err != nil {
		t.Fatalf("identity.json: %v", err)
	} else if st.Mode().Perm()&0o077 != 0 && os.Getenv("OS") != "Windows_NT" {
		t.Fatalf("identity.json should be 0600, got %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(a.Home, "a2a", "protocol.md")); err != nil {
		t.Fatalf("protocol.md: %v", err)
	}

	// non-friends cannot send directly
	if _, err := a.Send(context.Background(), b.Fingerprint(), "sneak", ""); err != ErrNotFriend {
		t.Fatalf("Send to a non-friend should return ErrNotFriend, got %v", err)
	}
	// cannot add yourself
	ac, _ := a.Card()
	if _, err := a.AddFriend(context.Background(), ac.EncodeURI(), ""); err != ErrSelf {
		t.Fatalf("adding yourself should be ErrSelf, got %v", err)
	}

	befriend(t, a, b)

	// A → B text
	res, err := a.Send(context.Background(), b.Fingerprint(), "first", "")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Status != "sent" || res.ID == "" {
		t.Fatalf("unexpected SendResult: %+v", res)
	}
	ev := b.await(t, "message.received", kindIs(EventMessageReceived))
	if ev.Peer != a.Fingerprint() || ev.Message.Body != "first" || ev.Message.ID != res.ID || ev.Seq != 1 {
		t.Fatalf("unexpected message.received: %+v", ev)
	}
	// B → A text (non-ASCII body on purpose: UTF-8 must round-trip through the wire and the archive)
	if _, err := b.Send(context.Background(), a.Fingerprint(), "回你一条 ✓", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ev2 := a.await(t, "message.received", kindIs(EventMessageReceived))
	if ev2.Message.Body != "回你一条 ✓" {
		t.Fatalf("A received the wrong message: %+v", ev2)
	}

	// conversation from A's view: out + in, two entries; since filter
	conv := a.Conversation(b.Fingerprint(), 0, 0)
	if len(conv) != 2 || conv[0].Dir != "out" || conv[1].Dir != "in" || conv[1].Seq != 2 {
		t.Fatalf("unexpected conversation for A: %+v", conv)
	}
	if got := a.Conversation(b.Fingerprint(), 1, 0); len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("since=1 should leave only entry 2: %+v", got)
	}
	// unread / read cursor
	fl := a.FriendList()
	if len(fl) != 1 || fl[0].Unread != 1 || fl[0].Count != 2 || fl[0].Last == nil || fl[0].Last.Body != "回你一条 ✓" {
		t.Fatalf("unexpected FriendList summary: %+v", fl)
	}
	if err := a.MarkRead(b.Fingerprint(), 2); err != nil {
		t.Fatal(err)
	}
	if fl := a.FriendList(); fl[0].Unread != 0 {
		t.Fatalf("unread should be 0 after MarkRead: %+v", fl[0])
	}
	// dedupe: redelivering the same message must not archive it twice
	fr := a.Friends.Get(b.Fingerprint())
	dup := &a2a.Message{ID: res.ID, From: a.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: a2a.TypeText, Body: "first"}
	if err := a.sendMessage(context.Background(), fr.Card, dup); err != nil {
		t.Fatal(err)
	}
	// send a fresh message as a "barrier"; once it arrives, count B's conversation
	if _, err := a.Send(context.Background(), b.Fingerprint(), "barrier", ""); err != nil {
		t.Fatal(err)
	}
	b.await(t, "barrier", func(e Event) bool { return e.Kind == EventMessageReceived && e.Message.Body == "barrier" })
	if got := b.Conversation(a.Fingerprint(), 0, 0); len(got) != 3 { // in first, out reply, in barrier
		t.Fatalf("redelivery must not archive twice; B's conversation should have 3 entries, got %d: %+v", len(got), got)
	}
}

func TestChunkedFileTransfer(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)

	raw := make([]byte, 900*1024) // > 700KB → 2 chunks
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	// Non-ASCII file name on purpose: artifact names must survive the wire and the disk layout.
	const bigName = "报告.bin"
	file := filepath.Join(t.TempDir(), bigName)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := a.Send(context.Background(), b.Fingerprint(), "here is a large file", file)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Chunks != 2 || res.Status != "sent" {
		t.Fatalf("should be split into 2 chunks: %+v", res)
	}
	ann := b.await(t, "chunk announcement", kindIs(EventMessageReceived))
	if ann.Message.ArtifactID == "" || ann.Message.ChunkTotal != 2 || ann.Message.ArtifactName != bigName || ann.Message.Artifact != "" {
		t.Fatalf("unexpected announcement: %+v", ann.Message)
	}
	ready := b.await(t, "artifact.ready", kindIs(EventArtifactReady))
	if ready.ArtifactID != ann.Message.ArtifactID || ready.ArtifactName != bigName {
		t.Fatalf("unexpected artifact.ready: %+v", ready)
	}
	got, err := os.ReadFile(ready.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("reassembled content differs (%d vs %d bytes)", len(got), len(raw))
	}
	if p, err := b.ArtifactFile(a.Fingerprint(), ann.Message.ArtifactID, bigName); err != nil || p != ready.ArtifactPath {
		t.Fatalf("ArtifactFile: %v %s", err, p)
	}
	// the sender keeps a local copy too
	if _, err := a.ArtifactFile(b.Fingerprint(), ann.Message.ArtifactID, bigName); err != nil {
		t.Fatalf("sender should keep a local copy: %v", err)
	}
	// staging dir is gone
	if _, err := os.Stat(b.incomingDir(a.Fingerprint(), ann.Message.ArtifactID)); !os.IsNotExist(err) {
		t.Fatal("staging dir should be removed after reassembly")
	}
	// chunk records are not shown in the conversation
	for _, e := range b.Conversation(a.Fingerprint(), 0, 0) {
		if e.Type == a2a.TypeArtifactChunk {
			t.Fatal("Conversation must not return artifact_chunk")
		}
	}

	// small attachment: inline path
	small := filepath.Join(t.TempDir(), "small.txt")
	_ = os.WriteFile(small, []byte("hello inline"), 0o644)
	res2, err := a.Send(context.Background(), b.Fingerprint(), "", small)
	if err != nil || res2.Chunks != 0 {
		t.Fatalf("small attachment should go inline: %v %+v", err, res2)
	}
	ev := b.await(t, "inline attachment", func(e Event) bool { return e.Kind == EventMessageReceived && e.Message.ArtifactName == "small.txt" })
	if ev.ArtifactPath == "" || ev.Message.Artifact != "" {
		t.Fatalf("inline attachment should be on disk and the event must not carry base64: %+v", ev)
	}
	if got, _ := os.ReadFile(ev.ArtifactPath); string(got) != "hello inline" {
		t.Fatalf("unexpected inline attachment content: %q", got)
	}
}

func TestTypingAndPresence(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)

	if err := a.Typing(context.Background(), b.Fingerprint(), true); err != nil {
		t.Fatal(err)
	}
	ev := b.await(t, "typing on", kindIs(EventTyping))
	if !ev.On || ev.Peer != a.Fingerprint() || !b.PeerTyping(a.Fingerprint()) {
		t.Fatalf("unexpected typing on: %+v", ev)
	}
	if err := a.Typing(context.Background(), b.Fingerprint(), false); err != nil {
		t.Fatal(err)
	}
	ev = b.await(t, "typing off", kindIs(EventTyping))
	if ev.On || b.PeerTyping(a.Fingerprint()) {
		t.Fatalf("unexpected typing off: %+v", ev)
	}
	// typing is not archived
	if got := b.Conversation(a.Fingerprint(), 0, 0); len(got) != 0 {
		t.Fatalf("typing must not be archived: %+v", got)
	}

	// presence: both are long-polling → online; unknown fingerprint → false
	on := a.Presence([]string{b.Fingerprint(), "not-a-friend"})
	if !on[b.Fingerprint()] || on["not-a-friend"] {
		t.Fatalf("unexpected presence: %+v", on)
	}
	if all := a.Presence(nil); !all[b.Fingerprint()] {
		t.Fatalf("Presence(nil) should cover all friends: %+v", all)
	}
}

func TestOutboxQueuesWhenRelayDown(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	// Forge a friend whose relay is dead (port 1 is practically never listening).
	bID, err := a2a.NewIdentity(t.TempDir(), "dead-relay", []string{"http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	bc, _ := bID.Card()
	if err := a.Friends.Add(bc, "dead-relay"); err != nil {
		t.Fatal(err)
	}
	res, err := a.Send(context.Background(), bID.Fingerprint(), "are you there", "")
	if err != nil {
		t.Fatalf("an unreachable relay should queue, not fail: %v", err)
	}
	if res.Status != "queued" || a.OutboxLen() != 1 {
		t.Fatalf("should be queued with outbox=1: %+v outbox=%d", res, a.OutboxLen())
	}
	conv := a.Conversation(bID.Fingerprint(), 0, 0)
	if len(conv) != 1 || conv[0].Status != "queued" {
		t.Fatalf("conversation should record queued: %+v", conv)
	}
}

func TestDirectoryPublishQueryUnpublish(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")

	if err := a.Publish(nil); err != ErrNoProfile {
		t.Fatalf("Publish without a profile should be ErrNoProfile, got %v", err)
	}
	prof := &a2a.Profile{Tags: []string{"golang"}, Summary: "writes Go", Intro: "I write Go and relays", Accepting: true,
		Skills: []a2a.Skill{{ID: "go", Title: "Go development", Tags: []string{"golang"}, Desc: "backend"},
			{ID: "secret", Title: "hidden skill", Hidden: true}}}
	if err := a.Publish(prof); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !a.Published() {
		t.Fatal("should be flagged as published")
	}
	hits, err := b.DirectoryQuery([]string{"golang"}, "", 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(hits) != 1 || hits[0].Profile.Fingerprint != a.Fingerprint() {
		t.Fatalf("should find alice: %+v", hits)
	}
	for _, s := range hits[0].Profile.Skills {
		if s.Hidden || s.ID == "secret" {
			t.Fatal("hidden skills must not be published")
		}
	}
	hit, err := b.DirectoryFetch(a.Fingerprint())
	if err != nil || hit == nil || hit.Card.Name != "alice" {
		t.Fatalf("Fetch: %v %+v", err, hit)
	}
	if err := a.Unpublish(); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	hits, _ = b.DirectoryQuery([]string{"golang"}, "", 10)
	if len(hits) != 0 {
		t.Fatalf("should not be found after unpublish: %+v", hits)
	}
	if a.Published() {
		t.Fatal("should be flagged as unpublished")
	}
}

// RemoveFriend drops the friends.yaml entry only; the archive stays readable and the
// peer is treated as a stranger again (its text is dropped, a new request is pended).
func TestRemoveFriendKeepsArchive(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)
	if _, err := a.Send(context.Background(), b.Fingerprint(), "before", ""); err != nil {
		t.Fatal(err)
	}
	b.await(t, "before", kindIs(EventMessageReceived))

	if err := b.RemoveFriend("nobody"); err != ErrNotFriend {
		t.Fatalf("removing a stranger should be ErrNotFriend, got %v", err)
	}
	if err := b.RemoveFriend(a.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if b.Friends.IsFriend(a.Fingerprint()) || len(b.FriendList()) != 0 {
		t.Fatal("friend should be gone")
	}
	if got := b.Conversation(a.Fingerprint(), 0, 0); len(got) != 1 || got[0].Body != "before" || got[0].Seq != 1 {
		t.Fatalf("archive must survive removal: %+v", got)
	}
	// a still thinks they are friends; its text is now dropped by b (no event, no archive).
	if _, err := a.Send(context.Background(), b.Fingerprint(), "after", ""); err != nil {
		t.Fatal(err)
	}
	// a new friend request from b gets pended on a's side only after b re-adds; here just
	// check b re-adding a works (a already has b, so a's Add updates the card snapshot).
	ac, _ := a.Card()
	if _, err := b.AddFriend(context.Background(), ac.EncodeURI(), "again"); err != nil {
		t.Fatalf("re-adding after removal: %v", err)
	}
	req := a.await(t, "friend.request", kindIs(EventFriendRequest))
	if req.Peer != b.Fingerprint() {
		t.Fatalf("request peer: %+v", req)
	}
	time.Sleep(200 * time.Millisecond) // give "after" time to arrive (it must be dropped)
	if got := b.Conversation(a.Fingerprint(), 1, 0); len(got) != 0 {
		t.Fatalf("mail from a non-friend must be dropped, got %+v", got)
	}
}

// Seq reported by Send / events equals the physical line number and Conversation reads
// incrementally with the same numbering.
func TestSeqConsistency(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)
	var seqs []int
	for i := 0; i < 3; i++ {
		res, err := a.Send(context.Background(), b.Fingerprint(), "m", "")
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, res.Seq)
		ev := b.await(t, "m", kindIs(EventMessageReceived))
		if ev.Seq != i+1 {
			t.Fatalf("event seq #%d = %d", i+1, ev.Seq)
		}
	}
	if seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("send seqs: %v", seqs)
	}
	if got := a.Conversation(b.Fingerprint(), 2, 0); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Conversation(since=2): %+v", got)
	}
	if got := a.Conversation(b.Fingerprint(), 0, 2); len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("Conversation(limit=2): %+v", got)
	}
	if got := a.Conversation("stranger", 0, 0); got == nil || len(got) != 0 {
		t.Fatalf("unknown conversation must be [] not nil: %#v", got)
	}
	if err := a.MarkRead(b.Fingerprint(), 2); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityGuards(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "h"), "")
	if err != nil {
		t.Fatal(err)
	}
	if n.Relay != DefaultRelay {
		t.Fatalf("default relay should be %s", DefaultRelay)
	}
	if _, err := n.Card(); err != ErrNoIdentity {
		t.Fatalf("Card without identity should be ErrNoIdentity: %v", err)
	}
	if err := n.Run(context.Background()); err != ErrNoIdentity {
		t.Fatalf("Run without identity should be ErrNoIdentity: %v", err)
	}
	if _, err := n.CreateIdentity(""); err == nil {
		t.Fatal("empty name should fail")
	}
	id, err := n.CreateIdentity("carol")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.CreateIdentity("dave"); err == nil {
		t.Fatal("an existing identity must not be overwritten")
	}
	if id2, _ := n.EnsureIdentity("erin"); id2.Fingerprint() != id.Fingerprint() || id2.Name != "carol" {
		t.Fatal("EnsureIdentity must not change an existing identity")
	}
	// reopening the same directory loads the same identity
	n2, err := Init(n.Home, "")
	if err != nil {
		t.Fatal(err)
	}
	if n2.Fingerprint() != id.Fingerprint() {
		t.Fatal("reopening should load the same identity")
	}
}
