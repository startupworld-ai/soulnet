package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/peer"
	"github.com/startupworld-ai/soulnet/relay"
)

// rpcHarness simulates the host with io.Pipe: writes requests to stdin, reads
// responses/notifications from stdout.
type rpcHarness struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan error
	srv   *Server
}

func newHarness(t *testing.T, n *peer.Peer) *rpcHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(n, outW, "test")
	h := &rpcHarness{t: t, in: inW, lines: make(chan map[string]any, 64), done: make(chan error, 1), srv: srv}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("non-JSON frame on stdout: %q", sc.Text())
				continue
			}
			h.lines <- m
		}
	}()
	go func() {
		err := srv.Serve(context.Background(), inR)
		srv.StopLoop()
		_ = outW.Close()
		h.done <- err
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *rpcHarness) send(id int, method string, params any) {
	h.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	if _, err := h.in.Write(append(raw, '\n')); err != nil {
		h.t.Fatalf("write stdin: %v", err)
	}
}

// call sends a request and waits for the response with the same id (notifications seen
// on the way are stashed).
func (h *rpcHarness) call(id int, method string, params any) map[string]any {
	h.t.Helper()
	h.send(id, method, params)
	return h.awaitResponse(id)
}

func (h *rpcHarness) awaitResponse(id int) map[string]any {
	h.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m := <-h.lines:
			if v, ok := m["id"]; ok && v != nil && int(v.(float64)) == id {
				return m
			}
			if m["method"] != nil {
				h.stash(m)
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for response id=%d", id)
		}
	}
}

var stashed []map[string]any

func (h *rpcHarness) stash(m map[string]any) { stashed = append(stashed, m) }

// awaitNotify waits for one notification of method (checks the stash first).
func (h *rpcHarness) awaitNotify(method string) map[string]any {
	h.t.Helper()
	for i, m := range stashed {
		if m["method"] == method {
			stashed = append(stashed[:i], stashed[i+1:]...)
			return m["params"].(map[string]any)
		}
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m := <-h.lines:
			if m["method"] == method {
				return m["params"].(map[string]any)
			}
			if m["method"] != nil {
				h.stash(m)
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for notification %s", method)
		}
	}
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	if e, ok := m["error"]; ok && e != nil {
		t.Fatalf("expected success, got error: %v", e)
	}
	r, _ := m["result"].(map[string]any)
	if r == nil {
		t.Fatalf("response has no result: %v", m)
	}
	return r
}

func rpcErrCode(t *testing.T, m map[string]any) int {
	t.Helper()
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("expected an error, got: %v", m)
	}
	return int(e["code"].(float64))
}

func TestStdioRPCEndToEnd(t *testing.T) {
	stashed = nil
	rs, err := relay.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(rs.Handler())
	defer hs.Close()

	// The other side: uses the peer package directly (simulates a SoulMirror / another light peer).
	other, err := peer.Init(filepath.Join(t.TempDir(), "peer"), hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	other.Logf = func(f string, a ...any) { t.Logf("[peer] "+f, a...) }
	if _, err := other.EnsureIdentity("other"); err != nil {
		t.Fatal(err)
	}
	otherEvents := make(chan peer.Event, 32)
	other.OnEvent = func(ev peer.Event) { otherEvents <- ev }
	pctx, pcancel := context.WithCancel(context.Background())
	defer pcancel()
	go func() { _ = other.Run(pctx) }()
	otherCard, _ := other.Card()

	// Under test: the RPC-driven peer.
	n, err := peer.Init(filepath.Join(t.TempDir(), "me"), hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(f string, a ...any) { t.Logf("[me] "+f, a...) }
	h := newHarness(t, n)

	// card.get without an identity must be codeNoIdentity
	if code := rpcErrCode(t, h.call(1, "card.get", nil)); code != codeNoIdentity {
		t.Fatalf("card.get without identity should be %d, got %d", codeNoIdentity, code)
	}
	// initialize{name} creates the identity and starts the loop
	init := result(t, h.call(2, "initialize", map[string]any{"name": "me"}))
	if init["protocol"] != ProtocolName || init["identity"] == nil || init["running"] != true {
		t.Fatalf("unexpected initialize result: %v", init)
	}
	myFp := init["identity"].(map[string]any)["fingerprint"].(string)
	// creating again must be codeIdentityExists
	if code := rpcErrCode(t, h.call(3, "identity.create", map[string]any{"name": "again"})); code != codeIdentityExists {
		t.Fatalf("duplicate identity.create should be %d, got %d", codeIdentityExists, code)
	}
	// unknown method
	if code := rpcErrCode(t, h.call(4, "no.such", nil)); code != codeMethodNotFound {
		t.Fatalf("unknown method should be %d, got %d", codeMethodNotFound, code)
	}
	// bad JSON → parse error (id null)
	_, _ = h.in.Write([]byte("{not json\n"))
	if m := h.awaitResponse0(); rpcErrCode(t, m) != codeParse {
		t.Fatal("bad JSON should be a parse error")
	}
	// card.parse
	cp := result(t, h.call(5, "card.parse", map[string]any{"uri": otherCard.EncodeURI()}))
	if cp["fingerprint"] != other.Fingerprint() {
		t.Fatalf("unexpected card.parse fingerprint: %v", cp)
	}
	if code := rpcErrCode(t, h.call(6, "card.parse", map[string]any{"uri": "soulmirror://card?pk=x"})); code != codeBadCard {
		t.Fatalf("bad card should be %d, got %d", codeBadCard, code)
	}
	// sending to a non-friend → codeNotFriend
	if code := rpcErrCode(t, h.call(7, "message.send", map[string]any{"to": other.Fingerprint(), "body": "sneak"})); code != codeNotFriend {
		t.Fatalf("non-friend should be %d, got %d", codeNotFriend, code)
	}
	// friends.add → other receives the request → other accepts → we get a friend.accepted notification
	fr := result(t, h.call(8, "friends.add", map[string]any{"card_uri": otherCard.EncodeURI(), "note": "the other one"}))
	if fr["friend"].(map[string]any)["fingerprint"] != other.Fingerprint() {
		t.Fatalf("unexpected friends.add result: %v", fr)
	}
	var req peer.Event
	select {
	case req = <-otherEvents:
	case <-time.After(15 * time.Second):
		t.Fatal("other timed out waiting for the friend request")
	}
	if req.Kind != peer.EventFriendRequest || req.Peer != myFp {
		t.Fatalf("other did not receive a friend request: %+v", req)
	}
	if _, err := other.Accept(context.Background(), req.PendingID, "me"); err != nil {
		t.Fatal(err)
	}
	acc := h.awaitNotify(peer.EventFriendAccepted)
	if acc["peer"] != other.Fingerprint() {
		t.Fatalf("unexpected friend.accepted notification: %v", acc)
	}
	fl := result(t, h.call(9, "friends.list", nil))
	if len(fl["friends"].([]any)) != 1 {
		t.Fatalf("friends.list should have 1 entry: %v", fl)
	}
	// message.send → other receives it (non-ASCII body on purpose: UTF-8 must round-trip)
	sr := result(t, h.call(10, "message.send", map[string]any{"to": other.Fingerprint(), "body": "你好 other"}))
	if sr["status"] != "sent" {
		t.Fatalf("unexpected message.send result: %v", sr)
	}
	select {
	case ev := <-otherEvents:
		if ev.Kind != peer.EventMessageReceived || ev.Message.Body != "你好 other" {
			t.Fatalf("other received the wrong message: %+v", ev)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("other timed out waiting for the message")
	}
	// other replies → a message.received notification appears on our stdout
	if _, err := other.Send(context.Background(), myFp, "got it", ""); err != nil {
		t.Fatal(err)
	}
	nt := h.awaitNotify(peer.EventMessageReceived)
	if nt["message"].(map[string]any)["body"] != "got it" {
		t.Fatalf("unexpected message.received notification: %v", nt)
	}
	// typing notification
	_ = other.Typing(context.Background(), myFp, true)
	if ty := h.awaitNotify(peer.EventTyping); ty["on"] != true {
		t.Fatalf("unexpected typing notification: %v", ty)
	}
	// message.send with auto=true (an alter's automatic reply): the receiver sees the
	// A2A auto flag (its loop guard) and the local archive keeps it too.
	ar := result(t, h.call(30, "message.send", map[string]any{"to": other.Fingerprint(), "body": "auto reply", "auto": true}))
	if ar["status"] != "sent" {
		t.Fatalf("unexpected auto message.send result: %v", ar)
	}
	select {
	case ev := <-otherEvents:
		if ev.Kind != peer.EventMessageReceived || ev.Message.Body != "auto reply" || !ev.Message.Auto {
			t.Fatalf("other should receive the auto-flagged message: %+v", ev)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("other timed out waiting for the auto message")
	}
	// friends.card → the friend's card link, parseable back to the same fingerprint
	fc := result(t, h.call(31, "friends.card", map[string]any{"fp": other.Fingerprint()}))
	if fc["fingerprint"] != other.Fingerprint() {
		t.Fatalf("friends.card fingerprint mismatch: %v", fc)
	}
	parsed := result(t, h.call(32, "card.parse", map[string]any{"uri": fc["uri"]}))
	if parsed["fingerprint"] != other.Fingerprint() {
		t.Fatalf("friends.card uri should parse back to the friend: %v", parsed)
	}
	if code := rpcErrCode(t, h.call(33, "friends.card", map[string]any{"fp": "nobody"})); code != codeNotFriend {
		t.Fatalf("friends.card of a non-friend should be %d, got %d", codeNotFriend, code)
	}
	// conversation.get / markRead
	cg := result(t, h.call(11, "conversation.get", map[string]any{"fp": other.Fingerprint()}))
	if len(cg["entries"].([]any)) != 3 {
		t.Fatalf("conversation should have 3 entries: %v", cg)
	}
	if last := cg["entries"].([]any)[2].(map[string]any); last["auto"] != true || last["dir"] != "out" {
		t.Fatalf("the archived auto reply should carry auto=true: %v", last)
	}
	result(t, h.call(12, "conversation.markRead", map[string]any{"fp": other.Fingerprint(), "seq": 2}))
	// presence
	pr := result(t, h.call(13, "presence", map[string]any{"fps": []string{other.Fingerprint()}}))
	if pr["online"].(map[string]any)[other.Fingerprint()] != true {
		t.Fatalf("other should be online: %v", pr)
	}
	// notification-style request (no id) gets no response: change the note via friends.set, then check
	h.send(0, "noop-check", nil) // id=0 → method not found; just confirms the pipe is alive
	h.awaitResponse(0)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "friends.set", "params": map[string]any{"fp": other.Fingerprint(), "note": "renamed"}})
	_, _ = h.in.Write(append(raw, '\n'))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fl := result(t, h.call(14, "friends.list", nil))
		if fl["friends"].([]any)[0].(map[string]any)["note"] == "renamed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// friends.remove → list empty; removing again → codeNotFriend
	result(t, h.call(15, "friends.remove", map[string]any{"fp": other.Fingerprint()}))
	if fl := result(t, h.call(16, "friends.list", nil)); len(fl["friends"].([]any)) != 0 {
		t.Fatalf("friends.list after remove should be empty: %v", fl)
	}
	if code := rpcErrCode(t, h.call(17, "friends.remove", map[string]any{"fp": other.Fingerprint()})); code != codeNotFriend {
		t.Fatalf("removing a non-friend should be %d, got %d", codeNotFriend, code)
	}
	// conversation archive survives the removal and still reads incrementally
	cg2 := result(t, h.call(18, "conversation.get", map[string]any{"fp": other.Fingerprint(), "since": 1}))
	if len(cg2["entries"].([]any)) != 2 {
		t.Fatalf("conversation after remove/since=1 should have 2 entries: %v", cg2)
	}
	// shutdown → Serve returns after the response
	result(t, h.call(99, "shutdown", nil))
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Serve should return cleanly: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
	if n.Running() {
		t.Fatal("receive loop should stop after shutdown")
	}
}

// awaitResponse0 waits for a response with id null (used for parse errors).
func (h *rpcHarness) awaitResponse0() map[string]any {
	h.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m := <-h.lines:
			if v, ok := m["id"]; ok && v == nil {
				return m
			}
			if m["method"] != nil {
				h.stash(m)
			}
		case <-deadline:
			h.t.Fatalf("timed out waiting for the id=null response")
		}
	}
}

func TestStdinEOFStopsServe(t *testing.T) {
	n, err := peer.Init(filepath.Join(t.TempDir(), "me"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(string, ...any) {}
	h := newHarness(t, n)
	ig := result(t, h.call(1, "identity.get", nil))
	if ig["identity"] != nil {
		t.Fatalf("identity should be null without one: %v", ig)
	}
	_ = h.in.Close()
	select {
	case err := <-h.done:
		if err != io.EOF {
			t.Fatalf("closing stdin should return io.EOF, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after stdin was closed")
	}
}

func TestIdentitySignRequest(t *testing.T) {
	stashed = nil
	rs, err := relay.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(rs.Handler())
	defer hs.Close()

	n, err := peer.Init(filepath.Join(t.TempDir(), "me"), hs.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, n)

	// no identity yet → codeNoIdentity
	if code := rpcErrCode(t, h.call(1, "identity.signRequest", map[string]any{"method": "POST", "path": "/v2/pay/wallet.create", "ts": time.Now().UTC().Format(time.RFC3339)})); code != codeNoIdentity {
		t.Fatalf("expected %d, got %d", codeNoIdentity, code)
	}
	if _, err := n.EnsureIdentity("me"); err != nil {
		t.Fatal(err)
	}
	// missing params → invalid params
	if code := rpcErrCode(t, h.call(2, "identity.signRequest", map[string]any{"method": "POST"})); code != codeInvalidParams {
		t.Fatalf("expected invalid params, got %d", code)
	}
	// happy path: signature verifies against the identity public key
	res := result(t, h.call(3, "identity.signRequest", map[string]any{"method": "POST", "path": "/v2/pay/wallet.create", "ts": time.Now().UTC().Format(time.RFC3339)}))
	sig := res["signature"].(string)
	id := n.Identity()
	pub, err := id.EdPublic()
	if err != nil {
		t.Fatal(err)
	}
	// VerifyReq checks signature + timestamp skew; we can also verify directly.
	fp, err := a2a.VerifyReq(a2a.EncodeKey(pub), "POST", "/v2/pay/wallet.create", time.Now().UTC().Format(time.RFC3339), sig)
	if err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	if fp != id.Fingerprint() {
		t.Fatalf("fingerprint mismatch: %s vs %s", fp, id.Fingerprint())
	}
}
