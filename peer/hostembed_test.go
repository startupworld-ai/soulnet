package peer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// The seams a host uses when it embeds the peer as its whole transport: registered
// handlers for its own message types, the inbound / send hooks, ErrQueued, the generic
// SendMessage with archiving and attachments, heartbeats, and the persisted voices roster.

type hookLog struct {
	mu       sync.Mutex
	inbound  []string // message types seen by OnInbound
	before   []string
	after    []string
	queued   []bool
	handled  []string
	heartbts map[string]int
}

func (h *hookLog) snapshot(f func(*hookLog)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func TestHostHandlerAndHooksOverRelay(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	recordEvents(alice)
	bobEvents := recordEvents(bob)

	hl := &hookLog{heartbts: map[string]int{}}
	// bob is the "product host": it owns the task type and watches the seams.
	bob.Handle(a2a.TypeTask, func(msg *a2a.Message) error {
		hl.snapshot(func(h *hookLog) { h.handled = append(h.handled, msg.Type+":"+msg.Body) })
		return nil
	})
	bob.OnInbound = func(msg *a2a.Message) {
		hl.snapshot(func(h *hookLog) { h.inbound = append(h.inbound, msg.Type) })
	}
	bob.OnHeartbeat = func(kind string) { hl.snapshot(func(h *hookLog) { h.heartbts[kind]++ }) }
	alice.BeforeSend = func(to *a2a.Card, msg *a2a.Message) {
		hl.snapshot(func(h *hookLog) { h.before = append(h.before, msg.Type) })
		if msg.Type == a2a.TypeText {
			msg.Body = msg.Body + " (amended)"
		}
	}
	alice.AfterSend = func(to *a2a.Card, msg *a2a.Message, queued bool) {
		hl.snapshot(func(h *hookLog) { h.after = append(h.after, msg.Type); h.queued = append(h.queued, queued) })
	}

	bobCard := bob.Friends.Get(alice.Fingerprint()) // alice as seen by bob: not needed; we need bob's card as seen by alice
	_ = bobCard
	toBob := alice.Friends.Get(bob.Fingerprint()).Card

	// 1) A host-only type: the peer would ignore "task" mail from a friend by archiving it;
	//    bob's handler takes it instead and nothing is archived.
	task := &a2a.Message{Type: a2a.TypeTask, Body: "do the thing"}
	res, err := alice.SendMessage(context.Background(), toBob, task, MessageOptions{})
	if err != nil || res.Status != "sent" || res.Seq != 0 {
		t.Fatalf("SendMessage(task): %+v %v", res, err)
	}
	if task.From != alice.Fingerprint() || task.To != bob.Fingerprint() || task.ID == "" || task.TS.IsZero() {
		t.Fatalf("SendMessage must complete From/To/ID/TS: %+v", task)
	}
	waitUntil(t, "bob's task handler ran", func() bool {
		var ok bool
		hl.snapshot(func(h *hookLog) { ok = len(h.handled) == 1 && h.handled[0] == "task:do the thing" })
		return ok
	})
	if n := len(bob.Conversation(alice.Fingerprint(), 0, 0)); n != 0 {
		t.Fatalf("a handled type must not be archived by the peer, got %d entries", n)
	}

	// 2) A text: BeforeSend amended it, it was archived on both sides with the amendment,
	//    OnInbound saw it before dispatch, message.received carries it.
	res, err = alice.SendMessage(context.Background(), toBob, &a2a.Message{Type: a2a.TypeText, Body: "hello"}, MessageOptions{Archive: true, SessionID: "sess-1"})
	if err != nil || res.Status != "sent" || res.Seq == 0 {
		t.Fatalf("SendMessage(text): %+v %v", res, err)
	}
	waitUntil(t, "bob archives the amended text", func() bool {
		return bobEvents.has(func(e Event) bool {
			return e.Kind == EventMessageReceived && e.Message != nil && e.Message.Body == "hello (amended)"
		})
	})
	out := alice.Conversation(bob.Fingerprint(), 0, 0)
	if len(out) != 1 || out[0].Body != "hello (amended)" || out[0].SessionID != "sess-1" || out[0].Status != "sent" {
		t.Fatalf("sender archive: %+v", out)
	}
	hl.snapshot(func(h *hookLog) {
		if len(h.before) != 2 || len(h.after) != 2 || h.queued[0] || h.queued[1] {
			t.Fatalf("send hooks: before=%v after=%v queued=%v", h.before, h.after, h.queued)
		}
		if len(h.inbound) < 2 || h.inbound[0] != a2a.TypeTask || h.inbound[1] != a2a.TypeText {
			t.Fatalf("OnInbound order: %v", h.inbound)
		}
		if h.heartbts[HeartbeatTick] == 0 || h.heartbts[HeartbeatPollOK] == 0 {
			t.Fatalf("heartbeats: %v", h.heartbts)
		}
	})

	// 3) An attachment above the chunk threshold goes out in chunks and lands whole.
	big := make([]byte, a2a.ChunkRawBytes*2+100)
	for i := range big {
		big[i] = byte(i)
	}
	res, err = alice.SendMessage(context.Background(), toBob, &a2a.Message{Type: a2a.TypeText, Body: "file"},
		MessageOptions{Attachment: &Attachment{Name: "big.bin", Raw: big}, Archive: true})
	if err != nil || res.Chunks < 2 {
		t.Fatalf("chunked SendMessage: %+v %v", res, err)
	}
	waitUntil(t, "bob reassembles the file", func() bool {
		return bobEvents.has(func(e Event) bool { return e.Kind == EventArtifactReady && e.ArtifactName == "big.bin" })
	})
}

func TestSendMessageQueuesWhenRelayDown(t *testing.T) {
	dead := "http://127.0.0.1:1"
	n, err := Init(filepath.Join(t.TempDir(), "home"), dead)
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(string, ...any) {}
	if _, err := n.EnsureIdentity("me"); err != nil {
		t.Fatal(err)
	}
	other, _ := a2a.NewIdentity(t.TempDir(), "other", []string{dead})
	card, _ := other.Card()
	var afterQueued *bool
	n.AfterSend = func(_ *a2a.Card, _ *a2a.Message, queued bool) { afterQueued = &queued }
	res, err := n.SendMessage(context.Background(), card, &a2a.Message{Type: a2a.TypeText, Body: "hi"}, MessageOptions{Archive: true})
	if !errors.Is(err, ErrQueued) {
		t.Fatalf("relay down must report ErrQueued, got %v", err)
	}
	if res == nil || res.Status != "queued" || res.Seq == 0 {
		t.Fatalf("queued result must still be returned and archived: %+v", res)
	}
	if n.OutboxLen() != 1 {
		t.Fatalf("outbox must hold the envelope, has %d", n.OutboxLen())
	}
	if afterQueued == nil || !*afterQueued {
		t.Fatal("AfterSend must report queued=true")
	}
	if got := n.Conversation(other.Fingerprint(), 0, 0); len(got) != 1 || got[0].Status != "queued" {
		t.Fatalf("archive must carry status=queued: %+v", got)
	}
	// SendWith keeps its contract: no error, status queued.
	if err := n.Friends.Add(card, "other"); err != nil {
		t.Fatal(err)
	}
	if r, err := n.SendWith(context.Background(), other.Fingerprint(), "again", SendOptions{}); err != nil || r.Status != "queued" {
		t.Fatalf("SendWith on a dead relay: %+v %v", r, err)
	}
}

func TestVoicesRosterPersistsAndSanitises(t *testing.T) {
	ug := newUnitGroup(t, "x", "y")
	n, gid := ug.n, ug.gid
	x, y := ug.other["x"].Fingerprint(), ug.other["y"].Fingerprint()
	n.SetGroupVoices(gid, x, []string{"DevBot", "Reviewer"})
	if got := n.GroupVoices(gid); len(got[x]) != 2 {
		t.Fatalf("roster after announce: %v", got)
	}
	if _, err := os.Stat(n.voicesPath(gid)); err != nil {
		t.Fatalf("voices.json must be written: %v", err)
	}
	// A fresh peer on the same home reads it back; a stranger's entry and oversized names
	// in the file are dropped on load.
	raw, _ := os.ReadFile(n.voicesPath(gid))
	_ = raw
	n2, err := Init(n.Home, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	n2.Logf = func(string, ...any) {}
	if got := n2.GroupVoices(gid); len(got[x]) != 2 || got[x][0] != "DevBot" {
		t.Fatalf("roster must survive a restart: %v", got)
	}
	// Clearing removes the entry and, once empty, the file.
	n2.SetGroupVoices(gid, x, nil)
	if got := n2.GroupVoices(gid); got != nil {
		t.Fatalf("cleared roster must be empty: %v", got)
	}
	if _, err := os.Stat(n2.voicesPath(gid)); !os.IsNotExist(err) {
		t.Fatal("empty roster must remove voices.json")
	}
	// Entries for non-members are ignored on load; the member cap holds on set.
	_ = os.WriteFile(n2.voicesPath(gid), []byte(`{"`+y+`":["Ops"],"stranger":["Ghost"]}`), 0o644)
	n3, _ := Init(n.Home, "http://127.0.0.1:1")
	n3.Logf = func(string, ...any) {}
	if got := n3.GroupVoices(gid); len(got) != 1 || len(got[y]) != 1 {
		t.Fatalf("loader must keep members only: %v", got)
	}
	for i := 0; i < MaxVoiceMembers+5; i++ {
		n3.SetGroupVoices(gid, "fp"+time.Now().Add(time.Duration(i)).Format(time.RFC3339Nano), []string{"Seat"})
	}
	if got := len(n3.GroupVoices(gid)); got != MaxVoiceMembers {
		t.Fatalf("member cap: want %d, got %d", MaxVoiceMembers, got)
	}
}

func TestGroupInfoMembersPreviewAndIndexCache(t *testing.T) {
	names := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		names = append(names, "m"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	ug := newUnitGroup(t, names...)
	n, gid := ug.n, ug.gid
	full, err := n.GroupInfo(gid)
	if err != nil || len(full.MemberList) != 31 || full.MemberTruncated || full.MyRole != "owner" || full.URI == "" {
		t.Fatalf("full view: %+v %v", full, err)
	}
	if full.MemberList[0].Fp != n.Fingerprint() {
		t.Fatal("the owner must lead the display order")
	}
	prev, err := n.GroupInfoMembers(gid, 5)
	if err != nil || len(prev.MemberList) != 5 || !prev.MemberTruncated || prev.Members != 31 {
		t.Fatalf("preview view: %+v %v", prev, err)
	}
	none, _ := n.GroupInfoMembers(gid, 0)
	if len(none.MemberList) != 0 || !none.MemberTruncated {
		t.Fatalf("preview 0 must carry no members: %+v", none)
	}
	page, err := n.GroupMembers(gid, "", 10, 10)
	if err != nil || len(page.Members) != 10 || page.Total != 31 || page.Matched != 31 || page.Offset != 10 {
		t.Fatalf("page: %+v %v", page, err)
	}
	x := ug.other["ma0"].Fingerprint()
	n.SetGroupVoices(gid, x, []string{"Zed"})
	hit, _ := n.GroupMembers(gid, "zed", 0, 0)
	if hit.Matched != 1 || hit.Members[0].Fp != x {
		t.Fatalf("search by seat name: %+v", hit)
	}
	if got := n.GroupNames(gid, []string{x, "nobody"}); len(got) != 1 || got[x] != "ma0" {
		t.Fatalf("names: %v", got)
	}
	// The roster index follows the version: a shorter roster at a higher version rebuilds it.
	st := n.Groups.Get(gid)
	if got := len(n.rosterIndexOf(st).order); got != 31 {
		t.Fatalf("index: %d", got)
	}
	st.Roster.Members = st.Roster.Members[:10]
	st.Roster.Version = 4
	if got := len(n.rosterIndexOf(st).order); got != 10 {
		t.Fatalf("index must rebuild on a version bump: %d", got)
	}
	// Invitable = my friends not on the roster.
	stranger, _ := a2a.NewIdentity(t.TempDir(), "friend-outside", []string{"http://127.0.0.1:1"})
	sc, _ := stranger.Card()
	_ = n.Friends.Add(sc, "outsider")
	inv, err := n.GroupInvitable(gid, "", 0, 0)
	if err != nil || inv.Total != 1 || inv.Members[0].Fp != stranger.Fingerprint() {
		t.Fatalf("invitable: %+v %v", inv, err)
	}
}
