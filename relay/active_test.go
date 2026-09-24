package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// deviceFixture is one relay + one identity whose mailbox the tests fight over.
type deviceFixture struct {
	s   *Server
	srv *httptest.Server
	id  *a2a.Identity
	box string
}

func newDeviceFixture(t *testing.T) *deviceFixture {
	t.Helper()
	s := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	id, err := a2a.NewIdentity(t.TempDir(), "owner", []string{srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return &deviceFixture{s: s, srv: srv, id: id, box: id.Fingerprint()}
}

// do sends an owner-signed request for method+path (query appended to the URL only), with
// the device header when device != "", and returns status + decoded JSON body.
func (f *deviceFixture) do(t *testing.T, method, path, query, device string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	priv, err := f.id.EdPrivate()
	if err != nil {
		t.Fatal(err)
	}
	req := signedReq(t, priv, method, f.srv.URL+path+query, path, raw)
	if device != "" {
		req.Header.Set(a2a.HeaderDevice, device)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *deviceFixture) poll(t *testing.T, device string) (int, map[string]any) {
	t.Helper()
	return f.do(t, "GET", "/mail", "?box="+f.box+"&wait=0", device, nil)
}

func (f *deviceFixture) ack(t *testing.T, device string) (int, map[string]any) {
	t.Helper()
	return f.do(t, "POST", "/mail/ack", "", device, map[string]any{"box": f.box, "ack_ids": []string{}})
}

func (f *deviceFixture) claim(t *testing.T, device, name string) (int, map[string]any) {
	t.Helper()
	return f.do(t, "POST", "/box/active", "", device, map[string]any{"box": f.box, "device": device, "name": name})
}

// send posts a letter FROM the fixture identity (to itself; the recipient does not matter
// for the gate, which keys on the sender) with the given device header.
func (f *deviceFixture) send(t *testing.T, device string) int {
	t.Helper()
	card, err := f.id.Card()
	if err != nil {
		t.Fatal(err)
	}
	env, err := a2a.SealEnvelope(f.id, card, &a2a.Message{ID: "m1", From: f.box, To: f.box, TS: time.Now(), Type: a2a.TypeText, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	req, _ := http.NewRequest("POST", f.srv.URL+"/mail", bytes.NewReader(raw))
	if device != "" {
		req.Header.Set(a2a.HeaderDevice, device)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func assertKicked(t *testing.T, code int, body map[string]any, wantDevice string) {
	t.Helper()
	if code != 409 {
		t.Fatalf("want 409 kicked, got %d %v", code, body)
	}
	if body["error"] != a2a.KickedErrorCode || body["active_device"] != wantDevice {
		t.Fatalf("kicked body must name the active device %q: %v", wantDevice, body)
	}
	if s, _ := body["since"].(string); s == "" {
		t.Fatalf("kicked body must carry since: %v", body)
	}
}

func TestDeviceFirstClaimThenSameDevicePasses(t *testing.T) {
	f := newDeviceFixture(t)
	if f.s.ActiveDevice(f.box) != nil {
		t.Fatal("a fresh mailbox has no active device")
	}
	if code, body := f.poll(t, "dev-A"); code != 200 {
		t.Fatalf("first device must be admitted: %d %v", code, body)
	}
	ad := f.s.ActiveDevice(f.box)
	if ad == nil || ad.Device != "dev-A" || ad.Since.IsZero() {
		t.Fatalf("first poll with a device header must claim the mailbox: %+v", ad)
	}
	if code, _ := f.poll(t, "dev-A"); code != 200 {
		t.Fatalf("the active device must keep passing: %d", code)
	}
	if code, _ := f.ack(t, "dev-A"); code != 200 {
		t.Fatalf("the active device must be able to ack: %d", code)
	}
	if code := f.send(t, "dev-A"); code != 200 {
		t.Fatalf("the active device must be able to send: %d", code)
	}
	// The claim is on disk, not only in memory.
	if _, err := os.Stat(filepath.Join(f.s.DataDir(), "active", f.box+".json")); err != nil {
		t.Fatalf("active.json must be persisted: %v", err)
	}
}

func TestDeviceOtherDeviceIsKickedOnPollAckAndSend(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.poll(t, "dev-A"); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	code, body := f.poll(t, "dev-B")
	assertKicked(t, code, body, "dev-A")
	code, body = f.ack(t, "dev-B")
	assertKicked(t, code, body, "dev-A")
	if code := f.send(t, "dev-B"); code != 409 {
		t.Fatalf("sending AS the identity from another device must be refused: %d", code)
	}
	// Being refused must not have moved the mailbox.
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-A" {
		t.Fatalf("a kicked device must not become active: %+v", ad)
	}
	// No letter was delivered by the refused send.
	if items, _ := f.s.readInbox(f.box); len(items) != 0 {
		t.Fatalf("refused send must not deliver: %d letters", len(items))
	}
}

func TestDeviceLegacyClientPassesUntilSomeoneClaims(t *testing.T) {
	f := newDeviceFixture(t)
	// No header, nothing claimed: exactly the old behaviour, and no claim is recorded.
	if code, _ := f.poll(t, ""); code != 200 {
		t.Fatalf("legacy poll on an unclaimed mailbox must pass: %d", code)
	}
	if code, _ := f.ack(t, ""); code != 200 {
		t.Fatalf("legacy ack on an unclaimed mailbox must pass: %d", code)
	}
	if code := f.send(t, ""); code != 200 {
		t.Fatalf("legacy send on an unclaimed mailbox must pass: %d", code)
	}
	if f.s.ActiveDevice(f.box) != nil {
		t.Fatal("legacy requests must never claim the mailbox")
	}
	// A device claims it: the legacy copy is kicked everywhere from now on.
	if code, body := f.claim(t, "dev-A", "Office desktop"); code != 200 {
		t.Fatalf("claim: %d %v", code, body)
	}
	code, body := f.poll(t, "")
	assertKicked(t, code, body, "dev-A")
	if body["active_name"] != "Office desktop" {
		t.Fatalf("kicked body must carry the device name: %v", body)
	}
	code, body = f.ack(t, "")
	assertKicked(t, code, body, "dev-A")
	if code := f.send(t, ""); code != 409 {
		t.Fatalf("legacy send after a claim must be refused: %d", code)
	}
}

func TestDeviceStrangerDeliveryIsNotGatedByRecipient(t *testing.T) {
	// The gate keys on the SENDER's mailbox: a letter from another identity to a claimed
	// mailbox must still be delivered (postal mail: anyone may drop a letter in).
	f := newDeviceFixture(t)
	if code, _ := f.poll(t, "dev-A"); code != 200 {
		t.Fatalf("setup: %d", code)
	}
	other, err := a2a.NewIdentity(t.TempDir(), "stranger", []string{f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	card, _ := f.id.Card()
	env, err := a2a.SealEnvelope(other, card, &a2a.Message{ID: "s1", From: other.Fingerprint(), To: f.box, TS: time.Now(), Type: a2a.TypeText, Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	resp, err := http.Post(f.srv.URL+"/mail", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a stranger's legacy delivery must pass: %d", resp.StatusCode)
	}
	if items, _ := f.s.readInbox(f.box); len(items) != 1 {
		t.Fatalf("the letter must be in the box: %d", len(items))
	}
}

func TestDeviceClaimWakesLongPollerWithKicked(t *testing.T) {
	f := newDeviceFixture(t)
	var events []Event
	var evMu sync.Mutex
	cancel := f.s.Subscribe(func(ev Event) {
		if ev.Kind == EventBoxActiveChanged {
			evMu.Lock()
			events = append(events, ev)
			evMu.Unlock()
		}
	})
	defer cancel()

	// dev-A holds a long poll (wait=20) on the empty mailbox.
	type res struct {
		code int
		body map[string]any
	}
	done := make(chan res, 1)
	go func() {
		code, body := f.do(t, "GET", "/mail", "?box="+f.box+"&wait=20", "dev-A", nil)
		done <- res{code, body}
	}()
	// Wait until the poller is parked (its implicit claim shows up), then take over.
	deadline := time.Now().Add(5 * time.Second)
	for f.s.ActiveDevice(f.box) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := f.claim(t, "dev-B", "Laptop"); code != 200 {
		t.Fatalf("takeover: %d %v", code, body)
	}
	start := time.Now()
	select {
	case r := <-done:
		assertKicked(t, r.code, r.body, "dev-B")
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("the long-poller must be woken by the takeover, not by its timeout (%s)", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poller was not woken by the takeover")
	}
	// GET /box/active reports the new holder ...
	code, body := f.do(t, "GET", "/box/active", "?box="+f.box, "dev-B", nil)
	if code != 200 {
		t.Fatalf("GET /box/active: %d %v", code, body)
	}
	active, _ := body["active"].(map[string]any)
	if active["device"] != "dev-B" || active["name"] != "Laptop" {
		t.Fatalf("unexpected active: %v", body)
	}
	// ... and the event trail is: implicit claim by A, takeover by B (previous = A).
	evMu.Lock()
	defer evMu.Unlock()
	if len(events) != 2 || events[0].Data["device"] != "dev-A" || events[1].Data["device"] != "dev-B" || events[1].Data["previous"] != "dev-A" {
		t.Fatalf("unexpected box.active_changed trail: %+v", events)
	}
}

func TestDeviceActiveSurvivesRestart(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.claim(t, "dev-A", "Office desktop"); code != 200 {
		t.Fatal("claim")
	}
	// A fresh Server on the same data directory (relay restart).
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	ad := s2.ActiveDevice(f.box)
	if ad == nil || ad.Device != "dev-A" || ad.Name != "Office desktop" {
		t.Fatalf("active device must be reloaded from disk after a restart: %+v", ad)
	}
	srv2 := httptest.NewServer(s2.Handler())
	defer srv2.Close()
	f2 := &deviceFixture{s: s2, srv: srv2, id: f.id, box: f.box}
	code, body := f2.poll(t, "dev-B")
	assertKicked(t, code, body, "dev-A")
	if code, _ := f2.poll(t, "dev-A"); code != 200 {
		t.Fatalf("the holder must still pass after a restart: %d", code)
	}
}

func TestDeviceClaimValidation(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.do(t, "POST", "/box/active", "", "", map[string]any{"box": f.box, "device": "has space", "name": "x"}); code != 400 {
		t.Fatalf("invalid device id must be 400, got %d", code)
	}
	if code, _ := f.do(t, "POST", "/box/active", "", "", map[string]any{"box": f.box, "device": "", "name": "x"}); code != 400 {
		t.Fatalf("empty device id must be 400, got %d", code)
	}
	// Signed by someone else than the box owner.
	other, err := a2a.NewIdentity(t.TempDir(), "other", []string{f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	priv, err := other.EdPrivate()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"box": f.box, "device": "dev-X", "name": "x"})
	req := signedReq(t, priv, "POST", f.srv.URL+"/box/active", "/box/active", raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("a claim signed by another key must be 401, got %d", resp.StatusCode)
	}
	if f.s.ActiveDevice(f.box) != nil {
		t.Fatal("rejected claims must not touch the mailbox")
	}
	// Unsigned GET /box/active is refused too (the holder is the owner's business).
	resp, err = http.Get(f.srv.URL + "/box/active?box=" + f.box)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unsigned GET /box/active must be 401, got %d", resp.StatusCode)
	}
	// Re-claiming with the same device only refreshes the name (no takeover, no event).
	if code, _ := f.claim(t, "dev-A", "first"); code != 200 {
		t.Fatal("claim")
	}
	n := 0
	cancel := f.s.Subscribe(func(ev Event) {
		if ev.Kind == EventBoxActiveChanged {
			n++
		}
	})
	defer cancel()
	if code, _ := f.claim(t, "dev-A", "renamed"); code != 200 {
		t.Fatal("re-claim")
	}
	if ad := f.s.ActiveDevice(f.box); ad.Name != "renamed" || n != 0 {
		t.Fatalf("same-device re-claim must rename without an event: %+v events=%d", ad, n)
	}
}
