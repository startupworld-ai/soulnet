package peer

// TEMPORARY debug harness: drives a real relay (httptest) + two peers through
// create → invite → join → chat, so the [relay-debug]/[recv-debug]/[grp-debug]
// log lines trace one group message end to end. DELETE before committing.

import (
	"context"
	"testing"
)

func TestZZGroupDebugLocal(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	befriend(t, alice, bob)
	ctx := context.Background()

	view, err := alice.GroupCreate(ctx, "debug群", []string{bob.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	t.Logf("=== group created gid=%s ===", gid)

	waitUntil(t, "bob joins", func() bool { return bob.Groups.Get(gid) != nil })

	if _, err := bob.GroupSend(ctx, gid, "hello from bob", GroupSendOptions{}); err != nil {
		t.Fatalf("bob GroupSend: %v", err)
	}
	alice.await(t, "alice hears bob", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "hello from bob"
	})

	if _, err := alice.GroupSend(ctx, gid, "hello from alice", GroupSendOptions{}); err != nil {
		t.Fatalf("alice GroupSend: %v", err)
	}
	bob.await(t, "bob hears alice", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.Body == "hello from alice"
	})
	t.Logf("=== round-trip complete ===")
}
