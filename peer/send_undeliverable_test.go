package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// TestPairwiseAttachmentSizesReachTheRelay sends files right at the inline limit (with a
// long text in the same message) and at the size that used to fail (690 KiB, inline
// under the old 700 KiB cap and ~1.2 MB on the wire) through a real relay: both must
// arrive intact, the first inline and the second chunked.
func TestPairwiseAttachmentSizesReachTheRelay(t *testing.T) {
	rl := startRelay(t)
	a := newTestNode(t, rl, "alice")
	b := newTestNode(t, rl, "bob")
	befriend(t, a, b)

	cases := []struct {
		name   string
		size   int
		body   string
		chunks int
	}{
		{"edge.bin", a2a.MaxArtifactBytes - 8*1024, strings.Repeat("长", 8*1024/3), 0},
		{"690k.bin", 690 * 1024, "", 2},
	}
	for _, c := range cases {
		raw := make([]byte, c.size)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		msg := &a2a.Message{Type: a2a.TypeText, Body: c.body}
		fr := a.Friends.Get(b.Fingerprint())
		res, err := a.SendMessage(context.Background(), fr.Card, msg, MessageOptions{Attachment: &Attachment{Name: c.name, Raw: raw}, Archive: true})
		if err != nil {
			t.Fatalf("%s: SendMessage: %v", c.name, err)
		}
		if res.Status != "sent" || res.Chunks != c.chunks {
			t.Fatalf("%s: want sent with %d chunks, got %+v", c.name, c.chunks, res)
		}
		if a.OutboxLen() != 0 {
			t.Fatalf("%s: nothing may be left in the outbox, got %d", c.name, a.OutboxLen())
		}
		name := c.name
		ev := b.await(t, name, func(e Event) bool {
			return (e.Kind == EventArtifactReady && e.ArtifactName == name) ||
				(e.Kind == EventMessageReceived && e.Message.ArtifactName == name && e.ArtifactPath != "")
		})
		got, err := os.ReadFile(ev.ArtifactPath)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("%s: received %d bytes (err %v), want %d", name, len(got), err, len(raw))
		}
	}
}

// fakeRelay answers POST /mail with a fixed status and counts the posts.
type fakeRelay struct {
	*httptest.Server
	status atomic.Int32
	posts  atomic.Int32
}

func newFakeRelay(t *testing.T, status int) *fakeRelay {
	t.Helper()
	f := &fakeRelay{}
	f.status.Store(int32(status))
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		code := int(f.status.Load())
		w.WriteHeader(code)
		if code == 200 {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"invalid envelope JSON"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

// newOfflinePeer creates a peer with an identity but no receive loop (the test drives
// flushOutbox itself) and returns it with the card of a second identity whose relay list
// is replaced by proxies.
func newOfflinePeer(t *testing.T, proxies ...string) (*Peer, *a2a.Card) {
	t.Helper()
	n, err := Init(filepath.Join(t.TempDir(), "a"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = t.Logf
	if _, err := n.EnsureIdentity("a"); err != nil {
		t.Fatal(err)
	}
	other, err := Init(filepath.Join(t.TempDir(), "b"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.EnsureIdentity("b"); err != nil {
		t.Fatal(err)
	}
	card, err := other.Card()
	if err != nil {
		t.Fatal(err)
	}
	card.Proxies = proxies
	return n, card
}

func TestRefusedEnvelopeIsNotQueued(t *testing.T) {
	for _, status := range []int{400, 401, 413} {
		f := newFakeRelay(t, status)
		n, card := newOfflinePeer(t, f.URL)
		res, err := n.SendMessage(context.Background(), card, &a2a.Message{Type: a2a.TypeText, Body: "x"}, MessageOptions{Archive: true})
		if !errors.Is(err, ErrUndeliverable) || res != nil {
			t.Fatalf("%d: want ErrUndeliverable and no result, got %v / %+v", status, err, res)
		}
		var re *a2a.RelayError
		if !errors.As(err, &re) || re.StatusCode != status {
			t.Fatalf("%d: the relay verdict must stay reachable, got %v", status, err)
		}
		if n.OutboxLen() != 0 {
			t.Fatalf("%d: a refused envelope must not be queued", status)
		}
		fp, _ := card.Fingerprint()
		if got := n.Conversation(fp, 0, 0); len(got) != 0 {
			t.Fatalf("%d: a refused message must not be archived as sent/queued: %+v", status, got)
		}
		if f.posts.Load() != 1 {
			t.Fatalf("%d: want exactly one post, got %d", status, f.posts.Load())
		}
	}
}

func TestTemporaryFailuresAreQueued(t *testing.T) {
	for _, status := range []int{408, 429, 500, 503} {
		f := newFakeRelay(t, status)
		n, card := newOfflinePeer(t, f.URL)
		res, err := n.SendMessage(context.Background(), card, &a2a.Message{Type: a2a.TypeText, Body: "x"}, MessageOptions{Archive: true})
		if !errors.Is(err, ErrQueued) || res == nil || res.Status != "queued" {
			t.Fatalf("%d: want queued, got %v / %+v", status, err, res)
		}
		if n.OutboxLen() != 1 {
			t.Fatalf("%d: want 1 queued envelope, got %d", status, n.OutboxLen())
		}
	}
	// Two relays, one refuses for good and one is unreachable: conservative, retry later.
	f := newFakeRelay(t, 400)
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	n, card := newOfflinePeer(t, f.URL, downURL)
	if _, err := n.SendMessage(context.Background(), card, &a2a.Message{Type: a2a.TypeText, Body: "x"}, MessageOptions{}); !errors.Is(err, ErrQueued) {
		t.Fatalf("one relay down must keep the envelope queued, got %v", err)
	}
}

// TestFlushOutboxDropsRefusedAndGoesOn: a refused envelope at the head of the queue is
// handed to OnUndeliverable and removed, and the envelope behind it is still sent in the
// same round (no head-of-line blocking); a temporary failure keeps its file.
func TestFlushOutboxDropsRefusedAndGoesOn(t *testing.T) {
	bad := newFakeRelay(t, 200) // accepts while queueing below, then starts refusing
	good := newFakeRelay(t, 503)
	n, badCard := newOfflinePeer(t, bad.URL)
	_, goodCard := newOfflinePeer(t, good.URL)

	// Queue one envelope per card (the relays fail temporarily at send time).
	bad.status.Store(503)
	good.status.Store(503)
	for _, c := range []*a2a.Card{badCard, goodCard} {
		if _, err := n.SendMessage(context.Background(), c, &a2a.Message{Type: a2a.TypeText, Body: "x"}, MessageOptions{}); !errors.Is(err, ErrQueued) {
			t.Fatalf("setup: want queued, got %v", err)
		}
	}
	if n.OutboxLen() != 2 {
		t.Fatalf("setup: want 2 queued, got %d", n.OutboxLen())
	}

	var mu sync.Mutex
	var dropped []string
	var dropErr error
	n.OnUndeliverable = func(name string, item *a2a.OutboxItem, err error) {
		mu.Lock()
		defer mu.Unlock()
		dropped = append(dropped, name)
		dropErr = err
		if item == nil || item.Env == nil || item.Card == nil {
			t.Error("OnUndeliverable must get the queued item")
		}
	}

	// Temporary failure: both stay.
	if sent := n.flushOutbox(context.Background()); sent != 0 || n.OutboxLen() != 2 || len(dropped) != 0 {
		t.Fatalf("temporary failure: sent=%d queued=%d dropped=%v", sent, n.OutboxLen(), dropped)
	}

	bad.status.Store(400)
	good.status.Store(200)
	if sent := n.flushOutbox(context.Background()); sent != 1 {
		t.Fatalf("the envelope behind the refused one must go out in the same round, sent=%d", sent)
	}
	if n.OutboxLen() != 0 {
		t.Fatalf("outbox should be empty, got %d", n.OutboxLen())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dropped) != 1 || !errors.Is(dropErr, ErrUndeliverable) {
		t.Fatalf("want one OnUndeliverable call with ErrUndeliverable, got %v / %v", dropped, dropErr)
	}
	before := bad.posts.Load()
	n.flushOutbox(context.Background())
	if bad.posts.Load() != before {
		t.Fatal("a dropped envelope must never be posted again")
	}
}
