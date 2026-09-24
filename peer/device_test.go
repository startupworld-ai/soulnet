package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/relay"
)

// recordingRelay is a fake relay that records the device headers of every mailbox-owner
// request and answers 200 (or a canned kicked verdict when kick is set).
type recordingRelay struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenReq
	kick bool
}

type seenReq struct {
	method, path, device, name string
	body                       map[string]any
	signed                     bool
}

func newRecordingRelay(t *testing.T) *recordingRelay {
	t.Helper()
	rr := &recordingRelay{}
	rr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rr.mu.Lock()
		rr.seen = append(rr.seen, seenReq{
			method: r.Method, path: r.URL.Path,
			device: r.Header.Get(a2a.HeaderDevice), name: r.Header.Get(a2a.HeaderDeviceName),
			body: body, signed: r.Header.Get(a2a.HeaderSignature) != "",
		})
		kick := rr.kick
		rr.mu.Unlock()
		if kick {
			relay.WriteJSON(w, 409, map[string]any{"error": "kicked", "active_device": "dev-other", "active_name": "Laptop", "since": "2026-09-24T08:00:00Z"})
			return
		}
		switch r.URL.Path {
		case "/mail":
			if r.Method == "GET" {
				relay.WriteJSON(w, 200, map[string]any{"messages": []any{}})
				return
			}
		case "/box/active":
			relay.WriteJSON(w, 200, map[string]any{"ok": true, "active": map[string]any{"device": body["device"], "name": body["name"], "since": time.Now().UTC().Format(time.RFC3339)}})
			return
		}
		relay.WriteJSON(w, 200, map[string]any{"ok": true})
	}))
	t.Cleanup(rr.srv.Close)
	return rr
}

func (rr *recordingRelay) last() seenReq {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.seen[len(rr.seen)-1]
}

// newIdlePeer creates a peer with an identity but without starting the receive loop.
func newIdlePeer(t *testing.T, relayURL string) *Peer {
	t.Helper()
	n, err := Init(filepath.Join(t.TempDir(), "home"), relayURL)
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(format string, args ...any) { t.Logf(format, args...) }
	if _, err := n.EnsureIdentity("me"); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeviceHeaderOnMailboxOwnerRequests(t *testing.T) {
	rr := newRecordingRelay(t)
	ctx := context.Background()

	// Without a DeviceID: legacy shape, no device header anywhere.
	legacy := newIdlePeer(t, rr.srv.URL)
	pc := legacy.proxyClient()
	if _, err := pc.Poll(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := rr.last(); got.path != "/mail" || got.device != "" || got.name != "" || !got.signed {
		t.Fatalf("legacy poll must be signed and carry no device header: %+v", got)
	}
	if err := pc.Ack(ctx, []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if got := rr.last(); got.path != "/mail/ack" || got.device != "" {
		t.Fatalf("legacy ack must carry no device header: %+v", got)
	}

	// With a DeviceID: every mailbox-owner request carries it (and the name).
	n := newIdlePeer(t, rr.srv.URL)
	n.DeviceID, n.DeviceName = "dev-A", "Office desktop"
	pc = n.proxyClient()
	if _, err := pc.Poll(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := rr.last(); got.path != "/mail" || got.method != "GET" || got.device != "dev-A" || got.name != "Office desktop" || !got.signed {
		t.Fatalf("poll must carry the device header: %+v", got)
	}
	if err := pc.Ack(ctx, []string{"a1"}); err != nil {
		t.Fatal(err)
	}
	if got := rr.last(); got.path != "/mail/ack" || got.device != "dev-A" {
		t.Fatalf("ack must carry the device header: %+v", got)
	}
	card, _ := n.Card()
	env, err := n.seal(card, &a2a.Message{ID: "m1", From: n.Fingerprint(), To: n.Fingerprint(), TS: time.Now(), Type: a2a.TypeText, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.DeliverToCard(ctx, card, env); err != nil {
		t.Fatal(err)
	}
	if got := rr.last(); got.path != "/mail" || got.method != "POST" || got.device != "dev-A" {
		t.Fatalf("deliver must carry the device header: %+v", got)
	}
}

func TestClaimActiveRequestShape(t *testing.T) {
	rr := newRecordingRelay(t)
	n := newIdlePeer(t, rr.srv.URL)
	if _, err := n.ClaimActive(context.Background()); err == nil {
		t.Fatal("ClaimActive without a DeviceID must fail")
	}
	n.DeviceID, n.DeviceName = "dev-A", "Office desktop"
	ad, err := n.ClaimActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := rr.last()
	if got.method != "POST" || got.path != "/box/active" || !got.signed {
		t.Fatalf("ClaimActive must POST a signed /box/active: %+v", got)
	}
	if got.body["box"] != n.Fingerprint() || got.body["device"] != "dev-A" || got.body["name"] != "Office desktop" {
		t.Fatalf("ClaimActive body must be {box, device, name}: %v", got.body)
	}
	if ad == nil || ad.Device != "dev-A" {
		t.Fatalf("ClaimActive must return the relay's active record: %+v", ad)
	}
}

func TestKickedVerdictBecomesErrKicked(t *testing.T) {
	rr := newRecordingRelay(t)
	rr.kick = true
	n := newIdlePeer(t, rr.srv.URL)
	n.DeviceID = "dev-A"
	_, err := n.proxyClient().Poll(context.Background(), 0)
	var k *ErrKicked
	if !errors.As(err, &k) {
		t.Fatalf("409 kicked must surface as *ErrKicked, got %T %v", err, err)
	}
	if k.ActiveDevice != "dev-other" || k.ActiveName != "Laptop" || k.Since.Format(time.RFC3339) != "2026-09-24T08:00:00Z" {
		t.Fatalf("ErrKicked must carry the relay's fields: %+v", k)
	}
	if !IsKicked(err) || IsKicked(&a2a.RelayError{StatusCode: 409, Message: "roster version must increase"}) {
		t.Fatal("IsKicked must key on the kicked verdict, not on any 409")
	}
	// A refused send is returned as is: not queued, not wrapped in ErrNetwork.
	card, _ := n.Card()
	_, err = n.SendMessage(context.Background(), card, &a2a.Message{Type: a2a.TypeText, Body: "x"}, MessageOptions{})
	if !IsKicked(err) || errors.Is(err, ErrQueued) || errors.Is(err, ErrNetwork) {
		t.Fatalf("a kicked send must return ErrKicked without queueing: %v", err)
	}
	if n.OutboxLen() != 0 {
		t.Fatal("a kicked send must not park the envelope in the outbox")
	}
}

// startRelayServer is startRelay that also hands back the server (to inspect the active device).
func startRelayServer(t *testing.T) (*relay.Server, string) {
	t.Helper()
	srv, err := relay.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return srv, hs.URL
}

func TestRunStopsWithDeviceKickedOnTakeover(t *testing.T) {
	rs, rl := startRelayServer(t)

	// Device A runs the identity.
	a := newIdlePeer(t, rl)
	a.DeviceID, a.DeviceName = "dev-A", "Desktop"
	events := make(chan Event, 16)
	a.OnEvent = func(ev Event) { events <- ev }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	// Its first poll claims the mailbox.
	deadline := time.Now().Add(5 * time.Second)
	for rs.ActiveDevice(a.Fingerprint()) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ad := rs.ActiveDevice(a.Fingerprint()); ad == nil || ad.Device != "dev-A" {
		t.Fatalf("the running device must have claimed the mailbox: %+v", ad)
	}
	if ok, err := a.IsActiveHere(context.Background()); err != nil || !ok {
		t.Fatalf("IsActiveHere on the holder: %v %v", ok, err)
	}

	// Device B holds the same key (a second copy of identity.json) and takes over.
	b, err := Init(filepath.Join(t.TempDir(), "home-b"), rl)
	if err != nil {
		t.Fatal(err)
	}
	b.Logf = a.Logf
	b.SetIdentity(a.Identity())
	b.DeviceID, b.DeviceName = "dev-B", "Laptop"
	if ok, _ := b.IsActiveHere(context.Background()); ok {
		t.Fatal("B is not the holder before claiming")
	}
	if _, err := b.ClaimActive(context.Background()); err != nil {
		t.Fatalf("ClaimActive: %v", err)
	}

	// A's loop stops with ErrKicked and tells its host.
	select {
	case err := <-runErr:
		if !IsKicked(err) {
			t.Fatalf("Run must return ErrKicked, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after the takeover")
	}
	var kicked *Event
	for kicked == nil {
		select {
		case ev := <-events:
			if ev.Kind == EventKicked {
				kicked = &ev
			}
		case <-time.After(2 * time.Second):
			t.Fatal("device.kicked was not emitted")
		}
	}
	if kicked.Kicked == nil || kicked.Kicked.ActiveDevice != "dev-B" || kicked.Kicked.ActiveName != "Laptop" {
		t.Fatalf("device.kicked must name the new holder: %+v", kicked.Kicked)
	}
	if a.Running() {
		t.Fatal("the loop must not be running any more")
	}
	if ad, err := a.ActiveDevice(context.Background()); err != nil || ad == nil || ad.Device != "dev-B" {
		t.Fatalf("ActiveDevice from A must report B: %+v %v", ad, err)
	}
}

func TestRendezvousRoundTripWithoutIdentity(t *testing.T) {
	_, rl := startRelayServer(t)
	// The joining device has no identity yet: rendezvous must work regardless.
	joiner, err := Init(filepath.Join(t.TempDir(), "joiner"), rl)
	if err != nil {
		t.Fatal(err)
	}
	if joiner.HasIdentity() {
		t.Fatal("precondition: no identity")
	}
	helper := newIdlePeer(t, rl)
	ctx := context.Background()
	const id = "pair-code-abcdef0123"

	// The joiner waits for the bundle; the helper puts two chunks.
	type res struct {
		items []a2a.RendezvousItem
		err   error
	}
	done := make(chan res, 1)
	go func() {
		items, err := joiner.RendezvousGet(ctx, id, 0, 10)
		done <- res{items, err}
	}()
	time.Sleep(300 * time.Millisecond)
	if err := helper.RendezvousPut(ctx, id, 1, []byte("chunk-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || len(r.items) != 1 || string(r.items[0].Data) != "chunk-1" {
			t.Fatalf("joiner must receive chunk 1: %+v %v", r.items, r.err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("joiner was not woken")
	}
	if err := helper.RendezvousPut(ctx, id, 2, []byte("chunk-2")); err != nil {
		t.Fatal(err)
	}
	items, err := joiner.RendezvousGet(ctx, id, 1, 0)
	if err != nil || len(items) != 1 || items[0].Seq != 2 {
		t.Fatalf("since semantics through the client: %+v %v", items, err)
	}
	if err := joiner.RendezvousDelete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if items, err := helper.RendezvousGet(ctx, id, 0, 0); err != nil || len(items) != 0 {
		t.Fatalf("deleted rendezvous must read empty: %+v %v", items, err)
	}
	// A relay error on the rendezvous surfaces as ErrNetwork with the status reachable.
	err = helper.RendezvousPut(ctx, "bad id", 1, []byte("x"))
	var re *a2a.RelayError
	if !errors.Is(err, ErrNetwork) || !errors.As(err, &re) || re.StatusCode != 400 {
		t.Fatalf("relay errors must wrap ErrNetwork and keep the status: %v", err)
	}
}
