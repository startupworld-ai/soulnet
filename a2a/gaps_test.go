package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Tests for the protocol-layer additions made while building the light peer
// (timeouts, handshake rule, message IDs, outbox, Since, OpenFrom, Remove, Presence,
// non-nil slices). Existing behaviour must be untouched: the older tests in this
// package and the vectors keep guarding that.

func newTestIdentity(t *testing.T, name string) *Identity {
	t.Helper()
	id, err := NewIdentity(t.TempDir(), name, []string{"https://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// --- 1. split timeouts ---

func TestProxyClientTimeouts(t *testing.T) {
	id := newTestIdentity(t, "a")
	pc := NewProxyClient("https://relay.example/", id)
	if pc.Base != "https://relay.example" {
		t.Fatalf("Base should be trimmed: %q", pc.Base)
	}
	if pc.ShortHTTP != nil {
		t.Fatal("NewProxyClient must leave ShortHTTP nil (behaviour unchanged)")
	}
	if pc.shortHTTP() != pc.HTTP {
		t.Fatal("short requests must fall back to HTTP when ShortHTTP is nil")
	}
	if pc.HTTP.Timeout != DefaultPollTimeout || DefaultPollTimeout != 70*time.Second {
		t.Fatalf("long-poll timeout must stay 70s, got %v", pc.HTTP.Timeout)
	}
	if got := pc.WithDeliverTimeout(0); got != pc {
		t.Fatal("WithDeliverTimeout must return the receiver for chaining")
	}
	if pc.ShortHTTP == nil || pc.ShortHTTP.Timeout != DefaultDeliverTimeout || DefaultDeliverTimeout != 15*time.Second {
		t.Fatalf("WithDeliverTimeout(0) should set the 15s default, got %+v", pc.ShortHTTP)
	}
	if pc.HTTP.Timeout != DefaultPollTimeout {
		t.Fatal("WithDeliverTimeout must not touch the long-poll client")
	}
	pc.WithDeliverTimeout(3 * time.Second)
	if pc.ShortHTTP.Timeout != 3*time.Second {
		t.Fatalf("custom deliver timeout not applied: %v", pc.ShortHTTP.Timeout)
	}

	// Deliver must honour ShortHTTP: a relay that stalls longer than the short timeout
	// fails fast even though the long-poll client would still be waiting.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slow.Close()
	pc = NewProxyClient(slow.URL, id).WithDeliverTimeout(50 * time.Millisecond)
	b := newTestIdentity(t, "b")
	bc, _ := b.Card()
	env, err := SealEnvelope(id, bc, &Message{ID: "x", From: id.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: TypeText, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := pc.Deliver(context.Background(), env); err == nil {
		t.Fatal("Deliver should time out via ShortHTTP")
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("Deliver waited %v — it used the long-poll client instead of ShortHTTP", time.Since(start))
	}
}

// --- 2. handshake rule ---

func TestSealEnvelopeHandshakeFromXPub(t *testing.T) {
	a, b := newTestIdentity(t, "a"), newTestIdentity(t, "b")
	bc, _ := b.Card()
	for _, typ := range []string{TypeFriendRequest, TypeFriendAccept} {
		env, err := SealEnvelope(a, bc, &Message{ID: "1", From: a.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		if env.FromXPub != a.XPub {
			t.Fatalf("%s envelope must carry from_xpub, got %q", typ, env.FromXPub)
		}
		if err := env.VerifyEnvelope(); err != nil {
			t.Fatalf("signature must still verify: %v", err)
		}
		// Receiver without a card can open it using only the envelope.
		bx, _ := b.XPrivate()
		theirX, _ := XPubFromB64(env.FromXPub)
		if _, err := OpenFrom(env, bx, theirX); err != nil {
			t.Fatalf("OpenFrom with from_xpub failed: %v", err)
		}
	}
	env, err := SealEnvelope(a, bc, &Message{ID: "2", From: a.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: TypeText, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Every pairwise envelope declares it now, so a friend who dropped us can still open the
	// mail and bounce a not_friend (peer/notfriend.go).
	if env.FromXPub != a.XPub {
		t.Fatalf("text envelope must carry from_xpub too, got %q", env.FromXPub)
	}
	if !IsHandshake(TypeFriendRequest) || !IsHandshake(TypeFriendAccept) || IsHandshake(TypeText) || IsHandshake(TypeTask) {
		t.Fatal("IsHandshake wrong")
	}
}

// --- 3. message IDs ---

func TestNewMessageID(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9_-]{1,6}-\d{19}-\d{12}$`)
	id := newTestIdentity(t, "a")
	fp := id.Fingerprint()
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 5000; i++ {
		m := NewMessageID(fp)
		if !re.MatchString(m) {
			t.Fatalf("bad id format: %q", m)
		}
		if !strings.HasPrefix(m, fp[:6]+"-") {
			t.Fatalf("id must start with the first 6 fingerprint chars: %q", m)
		}
		if seen[m] {
			t.Fatalf("duplicate id under burst: %q", m)
		}
		seen[m] = true
		if prev != "" && m <= prev {
			t.Fatalf("ids must be monotonic in lexical order: %q <= %q", m, prev)
		}
		prev = m
	}
	if got := NewMessageID("ab"); !strings.HasPrefix(got, "ab-") || !re.MatchString(got) {
		t.Fatalf("short fp should be used whole: %q", got)
	}
	if got := NewMessageID(""); !strings.HasPrefix(got, "-") {
		t.Fatalf("empty fp: %q", got)
	}
}

// --- 4. outbox ---

func TestOutboxReadWriteRemove(t *testing.T) {
	base := t.TempDir()
	dir := OutboxDir(base)
	if dir != filepath.Join(base, "a2a", "outbox") {
		t.Fatalf("OutboxDir: %s", dir)
	}
	if got, err := ReadOutbox(dir); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing outbox must read as empty non-nil: %v %v", got, err)
	}
	a, b := newTestIdentity(t, "a"), newTestIdentity(t, "b")
	bc, _ := b.Card()
	var names []string
	for i := 0; i < 3; i++ {
		env, _ := SealEnvelope(a, bc, &Message{ID: NewMessageID(a.Fingerprint()), From: a.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: TypeText, Body: "q"})
		name, err := WriteOutbox(dir, &OutboxItem{Card: bc, Env: env})
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^\d{19}-\d{12}\.json$`).MatchString(name) {
			t.Fatalf("outbox file name format: %q", name)
		}
		names = append(names, name)
	}
	if _, err := WriteOutbox(dir, &OutboxItem{Card: bc}); err == nil {
		t.Fatal("item without env must be rejected")
	}
	// Wire format: exactly {"card":…,"env":…} with the documented keys.
	raw, _ := os.ReadFile(filepath.Join(dir, names[0]))
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil || len(generic) != 2 || generic["card"] == nil || generic["env"] == nil {
		t.Fatalf("outbox file must be {card, env}: %s", raw)
	}
	// A corrupt file and a non-json file alongside.
	_ = os.WriteFile(filepath.Join(dir, "0000000000000000000-000000000000.json"), []byte("{nope"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644)
	entries, err := ReadOutbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("want 4 entries (3 good + 1 corrupt), got %d", len(entries))
	}
	if entries[0].Err == nil || entries[0].Item != nil {
		t.Fatalf("corrupt file must sort first and carry Err: %+v", entries[0])
	}
	for i, e := range entries[1:] {
		if e.Err != nil || e.Item == nil || e.Name != names[i] {
			t.Fatalf("entry %d: %+v want %s", i, e, names[i])
		}
		if e.Item.Env.To != b.Fingerprint() || e.Item.Card.EdPub != bc.EdPub {
			t.Fatalf("round trip lost fields: %+v", e.Item)
		}
	}
	if err := RemoveOutbox(dir, "../x.json"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if err := RemoveOutbox(dir, entries[0].Name); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOutbox(dir, entries[0].Name); err != nil {
		t.Fatalf("removing twice must be a no-op: %v", err)
	}
	if err := RemoveOutbox(dir, names[1]); err != nil {
		t.Fatal(err)
	}
	entries, _ = ReadOutbox(dir)
	if len(entries) != 2 || entries[0].Name != names[0] || entries[1].Name != names[2] {
		t.Fatalf("after removals: %+v", entries)
	}
}

// --- 5. Since / AppendSeq ---

func TestConvStoreSinceAndAppendSeq(t *testing.T) {
	dir := t.TempDir()
	cs := NewConvStore(dir)
	peer := "fpPeer"
	if got := cs.Since(peer, 0, 0); got == nil || len(got) != 0 {
		t.Fatalf("missing conversation must be empty non-nil: %v", got)
	}
	for i := 1; i <= 3; i++ {
		seq, err := cs.AppendSeq(peer, &ConvEntry{Dir: "in", Message: Message{ID: "m" + string(rune('0'+i)), Body: "b"}})
		if err != nil {
			t.Fatal(err)
		}
		if seq != i {
			t.Fatalf("AppendSeq #%d returned %d", i, seq)
		}
	}
	all := cs.Since(peer, 0, 0)
	if len(all) != 3 || all[0].Seq != 1 || all[2].Seq != 3 || all[2].ID != "m3" {
		t.Fatalf("Since(0): %+v", all)
	}
	if got := cs.Since(peer, 1, 0); len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("Since(1): %+v", got)
	}
	if got := cs.Since(peer, 0, 1); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Since(0, limit 1) must keep the last: %+v", got)
	}
	if got := cs.Since(peer, 3, 0); len(got) != 0 {
		t.Fatalf("Since(3) must be empty: %+v", got)
	}
	// seq must equal the physical line number, and the JSON shape is {"seq":n, …entry}.
	raw, _ := json.Marshal(all[1])
	if !strings.HasPrefix(string(raw), `{"seq":2,"dir":"in"`) {
		t.Fatalf("SeqEntry JSON shape: %s", raw)
	}
	// A corrupt line written behind the store's back still occupies a seq, and a fresh
	// store (cold cache) computes the next seq from the file.
	f, _ := os.OpenFile(filepath.Join(dir, "a2a", "conversations", peer, "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("{corrupt\n")
	_ = f.Close()
	cs2 := NewConvStore(dir)
	seq, err := cs2.AppendSeq(peer, &ConvEntry{Dir: "out", Message: Message{ID: "m5"}})
	if err != nil || seq != 5 {
		t.Fatalf("cold AppendSeq after corrupt line: seq=%d err=%v", seq, err)
	}
	got := cs2.Since(peer, 3, 0)
	if len(got) != 1 || got[0].Seq != 5 || got[0].ID != "m5" {
		t.Fatalf("corrupt line must be skipped but keep its seq: %+v", got)
	}
	// Recent (unchanged API) and Since agree on content.
	if rec := cs2.Recent(peer, 0); len(rec) != 4 || rec[3].ID != "m5" {
		t.Fatalf("Recent: %+v", rec)
	}
}

// --- 6. OpenFrom ---

func TestOpenFrom(t *testing.T) {
	a, b, c := newTestIdentity(t, "a"), newTestIdentity(t, "b"), newTestIdentity(t, "c")
	bc, _ := b.Card()
	bx, _ := b.XPrivate()
	aX, _ := XPubFromB64(a.XPub)
	env, err := SealEnvelope(a, bc, &Message{ID: "1", From: a.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: TypeText, Body: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := OpenFrom(env, bx, aX)
	if err != nil || msg.Body != "ok" {
		t.Fatalf("OpenFrom good path: %v %+v", err, msg)
	}
	// Tampered outer signature.
	bad := *env
	bad.Sig = EncodeKey([]byte(strings.Repeat("x", 64)))
	if _, err := OpenFrom(&bad, bx, aX); err == nil {
		t.Fatal("tampered signature must fail")
	}
	// Inner from != envelope signer: a seals a message claiming to be from c.
	spoof, _ := SealEnvelope(a, bc, &Message{ID: "2", From: c.Fingerprint(), To: b.Fingerprint(), TS: time.Now(), Type: TypeText, Body: "i am c"})
	if _, err := OpenFrom(spoof, bx, aX); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("spoofed inner from must be rejected, got %v", err)
	}
	// Plain Open still accepts it (unchanged behaviour) — OpenFrom is the strict one.
	if m, err := Open(bx, aX, spoof.Cipher); err != nil || m.From != c.Fingerprint() {
		t.Fatalf("Open must be unchanged: %v %+v", err, m)
	}
	if _, err := OpenFrom(nil, bx, aX); err == nil {
		t.Fatal("nil envelope")
	}
}

// --- 7. FriendStore.Remove + 9. non-nil slices ---

func TestFriendStoreRemoveAndNonNil(t *testing.T) {
	base := t.TempDir()
	fs := NewFriendStore(base)
	if got := fs.Friends(); got == nil || len(got) != 0 {
		t.Fatalf("Friends on empty store must be [] not nil: %#v", got)
	}
	if raw, _ := json.Marshal(fs.Friends()); string(raw) != "[]" {
		t.Fatalf("JSON of empty friends: %s", raw)
	}
	a, b := newTestIdentity(t, "a"), newTestIdentity(t, "b")
	ac, _ := a.Card()
	bc, _ := b.Card()
	_ = fs.Add(ac, "A")
	_ = fs.Add(bc, "B")
	if err := fs.Remove(a.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if fs.IsFriend(a.Fingerprint()) || !fs.IsFriend(b.Fingerprint()) || len(fs.Friends()) != 1 {
		t.Fatal("Remove must drop exactly that friend")
	}
	if err := fs.Remove(a.Fingerprint()); err == nil {
		t.Fatal("removing a non-friend must error")
	}
	// Persisted: a fresh store sees the removal.
	if NewFriendStore(base).IsFriend(a.Fingerprint()) {
		t.Fatal("removal not persisted")
	}
	ps := NewPendingStore(base)
	if got := ps.List(); got == nil || len(got) != 0 {
		t.Fatalf("PendingStore.List on empty must be [] not nil: %#v", got)
	}
	if raw, _ := json.Marshal(ps.List()); string(raw) != "[]" {
		t.Fatalf("JSON of empty pending: %s", raw)
	}
}

// --- 8. Presence ---

func TestProxyClientPresence(t *testing.T) {
	online := map[string]bool{"fpA": true, "fpB": false}
	var boxes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/presence" {
			http.NotFound(w, r)
			return
		}
		box := r.URL.Query().Get("box")
		boxes = append(boxes, box)
		if box == "boom" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"kaput"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"online": online[box]})
	}))
	defer srv.Close()
	pc := NewProxyClient(srv.URL, nil).WithDeliverTimeout(2 * time.Second) // presence needs no identity
	got, err := pc.Presence(context.Background(), []string{"fpA", "fpB", "fpC"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["fpA"] || got["fpB"] || got["fpC"] || len(got) != 3 {
		t.Fatalf("presence map: %+v", got)
	}
	if len(boxes) != 3 || boxes[0] != "fpA" {
		t.Fatalf("queried boxes: %v", boxes)
	}
	got, err = pc.Presence(context.Background(), []string{"boom", "fpA"})
	if err == nil || !strings.Contains(err.Error(), "kaput") {
		t.Fatalf("relay error must surface: %v", err)
	}
	if _, ok := got["boom"]; ok || !got["fpA"] {
		t.Fatalf("failed fp must be absent, others still answered: %+v", got)
	}
	if got, err := pc.Presence(context.Background(), nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty query: %v %v", got, err)
	}
}
