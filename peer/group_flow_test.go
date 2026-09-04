package peer

import (
	"context"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// waitUntil polls pred until true or fails the test.
func waitUntil(t *testing.T, what string, pred func() bool) {
	t.Helper()
	// 60s: matches `await` in peer_test.go — the peer's transient-retry budget
	// (~30s) for a sender key still in flight must fit inside the ceiling, or a
	// slow CI runner flakes.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}

// TestGroupCreateChatKickLeave drives the full group lifecycle over a real relay:
// create + auto-join on invite, sender-key distribution, fan-out both ways (including
// between members who are NOT friends), unread/markRead, kick + rekey shutting the
// removed member out, and leave shrinking the roster.
func TestGroupCreateChatKickLeave(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)
	// bob and carol are deliberately NOT friends: group delivery must not require it.

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "build club", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	if view.Members != 3 || !view.Mine {
		t.Fatalf("unexpected view: %+v", view)
	}
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })

	// Owner → group.
	res, err := alice.GroupSend(ctx, gid, "hello everyone", GroupSendOptions{})
	if err != nil || res.Status != "sent" {
		t.Fatalf("GroupSend: %+v %v", res, err)
	}
	evB := bob.await(t, "bob gets hello", func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid })
	if evB.Peer != alice.Fingerprint() || evB.Message.Body != "hello everyone" {
		t.Fatalf("bob got %+v", evB)
	}
	carol.await(t, "carol gets hello", func(e Event) bool { return e.Kind == EventGroupMessage && e.GID == gid })

	// Non-owner → group, posted by one of bob's NAMED seat agents (by=alter,
	// agent=DevBot): the display provenance must survive fan-out and archiving.
	if _, err := bob.GroupSend(ctx, gid, "hi from bob", GroupSendOptions{By: a2a.ByAlter, Auto: true, Agent: "DevBot"}); err != nil {
		t.Fatalf("bob GroupSend: %v", err)
	}
	alice.await(t, "alice gets bob's message", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "hi from bob" && e.Message.Agent == "DevBot"
	})
	carol.await(t, "carol gets bob's message", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "hi from bob" && e.Message.Agent == "DevBot"
	})

	// Archive + unread + markRead on carol; the agent name is archived too.
	entries := carol.GroupConversation(gid, 0, 0)
	if len(entries) != 2 {
		t.Fatalf("carol archive: want 2 entries, got %d", len(entries))
	}
	if entries[1].By != a2a.ByAlter || entries[1].Agent != "DevBot" {
		t.Fatalf("carol archived provenance: by=%q agent=%q", entries[1].By, entries[1].Agent)
	}

	// Voices metadata: bob announces his enabled seat agents; the other members'
	// member list carries them (not archived, so the entry count stays put).
	if err := bob.GroupAnnounceVoices(ctx, gid, []string{"DevBot", "Reviewer"}); err != nil {
		t.Fatalf("GroupAnnounceVoices: %v", err)
	}
	waitUntil(t, "alice sees bob's voices", func() bool {
		v, err := alice.GroupInfo(gid)
		if err != nil {
			return false
		}
		for _, m := range v.MemberList {
			if m.Fp == bob.Fingerprint() && len(m.Agents) == 2 && m.Agents[0] == "DevBot" {
				return true
			}
		}
		return false
	})
	if got := len(carol.GroupConversation(gid, 0, 0)); got != 2 {
		t.Fatalf("voices announce leaked into the archive: %d entries", got)
	}
	sums := carol.GroupList()
	if len(sums) != 1 || sums[0].GID != gid || sums[0].Unread != 2 {
		t.Fatalf("carol summaries: %+v", sums)
	}
	if err := carol.GroupMarkRead(gid, 0); err != nil {
		t.Fatalf("GroupMarkRead: %v", err)
	}
	if s := carol.GroupList(); s[0].Unread != 0 {
		t.Fatalf("unread after markRead: %d", s[0].Unread)
	}

	// Kick carol: her node keeps the group read-only (history intact, marked as removed);
	// the survivors rekey and keep chatting.
	if err := alice.GroupKick(ctx, gid, carol.Fingerprint()); err != nil {
		t.Fatalf("GroupKick: %v", err)
	}
	waitUntil(t, "carol is marked as removed", func() bool { return carol.GroupLeft(gid) })
	if carol.Groups.Get(gid) == nil {
		t.Fatal("a removed member must keep the group locally (read-only)")
	}
	waitUntil(t, "bob sees roster v2", func() bool {
		st := bob.Groups.Get(gid)
		return st != nil && st.Roster.Version == 2 && st.Roster.Member(carol.Fingerprint()) == nil
	})
	if _, err := alice.GroupSend(ctx, gid, "carol is gone", GroupSendOptions{}); err != nil {
		t.Fatalf("post-kick send: %v", err)
	}
	bob.await(t, "bob gets the post-kick message", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "carol is gone"
	})
	if got := len(carol.GroupConversation(gid, 0, 0)); got != 2 {
		t.Fatalf("carol saw post-kick traffic: %d entries", got)
	}

	// Kick permissions.
	if err := bob.GroupKick(ctx, gid, alice.Fingerprint()); err == nil {
		t.Fatalf("non-owner kick accepted")
	}

	// Bob leaves: alice shrinks the roster to herself; bob forgets the group.
	if err := bob.GroupLeave(ctx, gid); err != nil {
		t.Fatalf("GroupLeave: %v", err)
	}
	waitUntil(t, "alice's roster shrinks to 1", func() bool {
		st := alice.Groups.Get(gid)
		return st != nil && len(st.Roster.Members) == 1
	})
	if bob.Groups.Get(gid) != nil {
		t.Fatalf("bob still holds the group after leaving")
	}
	if err := alice.GroupLeave(ctx, gid); err == nil {
		t.Fatalf("owner leave accepted")
	}

	// Rekey proof: alice's sender key must be past epoch 1 after removals.
	if k := alice.Groups.Keys(gid); k.Mine == nil || k.Mine.Epoch < 2 {
		t.Fatalf("alice did not rekey: %+v", k.Mine)
	}
	_ = a2a.GroupConvKey(gid)
}
