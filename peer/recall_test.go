package peer

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// entryByID finds one archived entry (nil when absent).
func entryByID(es []Entry, id string) *Entry {
	for i := range es {
		if es[i].ID == id {
			return &es[i]
		}
	}
	return nil
}

// TestRecallPairwise: alice withdraws a text with an inline file; bob's archive keeps a
// wiped shell marked Recalled (file gone), alice keeps her text marked Recalled. Bob can
// not recall alice's message, and a forged recall (alice naming bob's message) is
// rejected by bob's node.
func TestRecallPairwise(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)
	ctx := context.Background()
	afp, bfp := a.Fingerprint(), b.Fingerprint()

	fr := a.Friends.Get(bfp)
	res, err := a.SendMessage(ctx, fr.Card, &a2a.Message{Type: a2a.TypeText, Body: "oops, wrong chat"},
		MessageOptions{Attachment: &Attachment{Name: "secret.txt", Raw: []byte("secret")}, Archive: true})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	got := b.await(t, "bob gets the text", kindIs(EventMessageReceived))
	if got.ArtifactPath == "" {
		t.Fatal("expected the inline attachment on disk")
	}
	artPath := got.ArtifactPath

	// Bob cannot recall alice's message (it is not his).
	if _, err := b.Recall(ctx, afp, res.ID); !errors.Is(err, ErrNotRecallable) {
		t.Fatalf("bob recalling alice's message: want ErrNotRecallable, got %v", err)
	}
	if _, err := a.Recall(ctx, bfp, "no-such-id"); !errors.Is(err, ErrNoMessage) {
		t.Fatalf("unknown id: want ErrNoMessage, got %v", err)
	}

	rr, err := a.Recall(ctx, bfp, res.ID)
	if err != nil || rr.Status != "sent" || rr.Seq != res.Seq {
		t.Fatalf("Recall: %+v %v (want seq %d)", rr, err, res.Seq)
	}
	ev := b.await(t, "bob gets the recall", kindIs(EventMessageRecalled))
	if ev.Peer != afp || ev.Recalled != res.ID || ev.GID != "" || ev.Seq != got.Seq {
		t.Fatalf("recall event: %+v (want seq %d)", ev, got.Seq)
	}
	in := entryByID(b.Conversation(afp, 0, 0), res.ID)
	if in == nil || !in.Recalled || in.Body != "" || in.ArtifactName != "" || in.Seq != got.Seq {
		t.Fatalf("bob's archived copy not wiped: %+v", in)
	}
	if _, err := os.Stat(artPath); !os.IsNotExist(err) {
		t.Fatalf("recalled attachment still on disk: %v", err)
	}
	out := entryByID(a.Conversation(bfp, 0, 0), res.ID)
	if out == nil || !out.Recalled || out.Body != "oops, wrong chat" {
		t.Fatalf("alice's copy should keep the text, marked recalled: %+v", out)
	}
	// Twice is a no-op.
	if rr2, err := a.Recall(ctx, bfp, res.ID); err != nil || rr2.ID != "" {
		t.Fatalf("second Recall: %+v %v", rr2, err)
	}

	// Forgery: bob writes, alice sends a recall naming BOB's message. Bob's node must
	// reject it (the message is not alice's); a follow-up proves it was processed.
	bres, err := b.Send(ctx, afp, "my own words", "")
	if err != nil {
		t.Fatal(err)
	}
	a.await(t, "alice gets bob's text", kindIs(EventMessageReceived))
	if _, err := a.SendMessage(ctx, a.Friends.Get(bfp).Card, &a2a.Message{Type: a2a.TypeRecall, Recall: bres.ID}, MessageOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send(ctx, bfp, "after the forgery", ""); err != nil {
		t.Fatal(err)
	}
	b.await(t, "bob gets the follow-up", func(e Event) bool {
		if e.Kind == EventMessageRecalled {
			t.Fatalf("forged recall was honoured: %+v", e)
		}
		return e.Kind == EventMessageReceived && e.Message.Body == "after the forgery"
	})
	if mine := entryByID(b.Conversation(afp, 0, 0), bres.ID); mine == nil || mine.Recalled || mine.Body != "my own words" {
		t.Fatalf("bob's own message was touched by a forged recall: %+v", mine)
	}
}

// TestRecallGroup: bob withdraws a post; alice and carol (carol is not bob's friend) get
// message.recalled and wipe it. A recall carol fans out naming bob's post is rejected.
func TestRecallGroup(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "recall club", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	res, err := bob.GroupSend(ctx, gid, "typo here", GroupSendOptions{})
	if err != nil || res.Status != "sent" {
		t.Fatalf("GroupSend: %+v %v", res, err)
	}
	isPost := func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid && e.Message.ID == res.ID }
	alice.await(t, "alice gets the post", isPost)
	carol.await(t, "carol gets the post", isPost)

	if _, err := alice.GroupRecall(ctx, gid, res.ID); !errors.Is(err, ErrNotRecallable) {
		t.Fatalf("alice recalling bob's post: want ErrNotRecallable, got %v", err)
	}
	rr, err := bob.GroupRecall(ctx, gid, res.ID)
	if err != nil || rr.Seq != res.Seq {
		t.Fatalf("GroupRecall: %+v %v", rr, err)
	}
	isRecall := func(e Event) bool { return e.Kind == EventMessageRecalled && e.GID == gid && e.Recalled == res.ID }
	for _, n := range []*testNode{alice, carol} {
		ev := n.await(t, "recall event", isRecall)
		if ev.Peer != bob.Fingerprint() {
			t.Fatalf("recall event peer: %+v", ev)
		}
		e := entryByID(n.GroupConversation(gid, 0, 0), res.ID)
		if e == nil || !e.Recalled || e.Body != "" {
			t.Fatalf("archived post not wiped: %+v", e)
		}
	}
	if e := entryByID(bob.GroupConversation(gid, 0, 0), res.ID); e == nil || !e.Recalled || e.Body != "typo here" {
		t.Fatalf("bob's own copy: %+v", e)
	}

	// Forgery: alice posts, carol fans out a recall naming alice's post.
	ares, err := alice.GroupSend(ctx, gid, "alice stays", GroupSendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bob.await(t, "bob gets alice's post", func(e Event) bool { return e.Kind == EventGroupMessage && e.Message.ID == ares.ID })
	st := carol.Groups.Get(gid)
	forged := &a2a.Message{ID: carol.newMsgID(), From: carol.Fingerprint(), GID: gid, TS: time.Now(), Type: a2a.TypeRecall, Recall: ares.ID}
	if err := carol.fanOutGroup(ctx, st, forged); err != nil {
		t.Fatal(err)
	}
	if _, err := carol.GroupSend(ctx, gid, "carol after", GroupSendOptions{}); err != nil {
		t.Fatal(err)
	}
	bob.await(t, "bob gets carol's follow-up", func(e Event) bool {
		if e.Kind == EventMessageRecalled {
			t.Fatalf("forged group recall was honoured: %+v", e)
		}
		return e.Kind == EventGroupMessage && e.Message.Body == "carol after"
	})
	if e := entryByID(bob.GroupConversation(gid, 0, 0), ares.ID); e == nil || e.Recalled || e.Body != "alice stays" {
		t.Fatalf("alice's post touched by a forged recall: %+v", e)
	}
}
