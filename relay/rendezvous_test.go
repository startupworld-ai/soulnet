package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rvFixture struct {
	s   *Server
	srv *httptest.Server
}

func newRvFixture(t *testing.T) *rvFixture {
	t.Helper()
	s := newTestServer(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &rvFixture{s: s, srv: srv}
}

func (f *rvFixture) put(t *testing.T, id string, seq int64, data []byte) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"seq": seq, "data": base64.StdEncoding.EncodeToString(data)})
	resp, err := http.Post(f.srv.URL+"/rendezvous/"+id, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

type rvItem struct {
	Seq  int64  `json:"seq"`
	Data []byte `json:"data"`
}

func (f *rvFixture) get(t *testing.T, id string, since int64, wait int) (int, []rvItem) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/rendezvous/%s?since=%d&wait=%d", f.srv.URL, id, since, wait))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Items []rvItem `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Items
}

func (f *rvFixture) del(t *testing.T, id string) int {
	t.Helper()
	req, _ := http.NewRequest("DELETE", f.srv.URL+"/rendezvous/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

const rvID = "pair-code-0123456789"

func TestRendezvousPutGetOrderAndSince(t *testing.T) {
	f := newRvFixture(t)
	// Out-of-order puts come back in seq order.
	for _, seq := range []int64{2, 1, 3} {
		if code := f.put(t, rvID, seq, []byte(fmt.Sprintf("blob-%d", seq))); code != 200 {
			t.Fatalf("put %d: %d", seq, code)
		}
	}
	code, items := f.get(t, rvID, 0, 0)
	if code != 200 || len(items) != 3 {
		t.Fatalf("get all: %d %+v", code, items)
	}
	for i, it := range items {
		if it.Seq != int64(i+1) || string(it.Data) != fmt.Sprintf("blob-%d", i+1) {
			t.Fatalf("item %d out of order or corrupted: %+v", i, it)
		}
	}
	// since is exclusive.
	if _, items := f.get(t, rvID, 2, 0); len(items) != 1 || items[0].Seq != 3 {
		t.Fatalf("since=2 must yield only seq 3: %+v", items)
	}
	if _, items := f.get(t, rvID, 3, 0); len(items) != 0 {
		t.Fatalf("since=3 must yield nothing: %+v", items)
	}
	// A duplicate seq is refused, the stored blob untouched.
	if code := f.put(t, rvID, 2, []byte("overwrite")); code != 409 {
		t.Fatalf("duplicate seq must be 409, got %d", code)
	}
	if _, items := f.get(t, rvID, 1, 0); string(items[0].Data) != "blob-2" {
		t.Fatalf("duplicate seq must not overwrite: %+v", items)
	}
	// Blobs are on disk under rendezvous/<id>/.
	if entries, _ := os.ReadDir(filepath.Join(f.s.DataDir(), "rendezvous", rvID)); len(entries) != 3 {
		t.Fatalf("expected 3 blob files on disk, got %d", len(entries))
	}
}

func TestRendezvousLongPollWakesOnPut(t *testing.T) {
	f := newRvFixture(t)
	// The reader arrives before the rendezvous exists (unknown id is not an error).
	type res struct {
		code  int
		items []rvItem
	}
	done := make(chan res, 1)
	go func() {
		code, items := f.get(t, rvID, 0, 10)
		done <- res{code, items}
	}()
	time.Sleep(400 * time.Millisecond) // let the poller park (and go through one "not created yet" round)
	start := time.Now()
	if code := f.put(t, rvID, 1, []byte("first")); code != 200 {
		t.Fatalf("put: %d", code)
	}
	select {
	case r := <-done:
		if r.code != 200 || len(r.items) != 1 || string(r.items[0].Data) != "first" {
			t.Fatalf("poller must return the new blob: %d %+v", r.code, r.items)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("poller must be woken by the put, not by its timeout (%s)", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("poller was not woken")
	}
	// A parked poller on an existing rendezvous is woken by the next put too.
	go func() {
		code, items := f.get(t, rvID, 1, 10)
		done <- res{code, items}
	}()
	time.Sleep(200 * time.Millisecond)
	if code := f.put(t, rvID, 2, []byte("second")); code != 200 {
		t.Fatalf("put: %d", code)
	}
	select {
	case r := <-done:
		if len(r.items) != 1 || r.items[0].Seq != 2 {
			t.Fatalf("poller must get exactly the blob after since: %+v", r.items)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("second poller was not woken")
	}
	// wait=0 on an empty tail answers immediately with an empty list.
	if code, items := f.get(t, rvID, 2, 0); code != 200 || len(items) != 0 {
		t.Fatalf("empty tail: %d %+v", code, items)
	}
}

func TestRendezvousDeleteAndIdleExpiry(t *testing.T) {
	f := newRvFixture(t)
	if code := f.put(t, rvID, 1, []byte("x")); code != 200 {
		t.Fatal("put")
	}
	if code := f.del(t, rvID); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code := f.del(t, rvID); code != 200 {
		t.Fatalf("delete must be idempotent: %d", code)
	}
	if _, items := f.get(t, rvID, 0, 0); len(items) != 0 {
		t.Fatalf("deleted rendezvous must read empty: %+v", items)
	}
	if _, err := os.Stat(filepath.Join(f.s.DataDir(), "rendezvous", rvID)); !os.IsNotExist(err) {
		t.Fatalf("delete must remove the blobs on disk: %v", err)
	}

	// Idle expiry: nothing touches the rendezvous for longer than rvIdle -> swept on the next request.
	f.s.rvIdle = 50 * time.Millisecond
	other := "pair-code-expiring-1"
	if code := f.put(t, other, 1, []byte("x")); code != 200 {
		t.Fatal("put")
	}
	if f.s.RendezvousCount() != 1 {
		t.Fatal("rendezvous must be alive right after the put")
	}
	time.Sleep(120 * time.Millisecond)
	if n := f.s.RendezvousCount(); n != 0 {
		t.Fatalf("idle rendezvous must be swept, %d alive", n)
	}
	if _, err := os.Stat(filepath.Join(f.s.DataDir(), "rendezvous", other)); !os.IsNotExist(err) {
		t.Fatalf("expiry must remove the blobs on disk: %v", err)
	}
	if _, items := f.get(t, other, 0, 0); len(items) != 0 {
		t.Fatalf("expired rendezvous must read empty: %+v", items)
	}
	// Activity keeps it alive: reads within the idle window reset the clock.
	f.s.rvIdle = 150 * time.Millisecond
	if code := f.put(t, other, 1, []byte("y")); code != 200 {
		t.Fatal("put")
	}
	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		f.get(t, other, 0, 0)
	}
	if f.s.RendezvousCount() != 1 {
		t.Fatal("a rendezvous that is being read must not expire")
	}
}

func TestRendezvousStartupDropsLeftovers(t *testing.T) {
	f := newRvFixture(t)
	if code := f.put(t, rvID, 1, []byte("x")); code != 200 {
		t.Fatal("put")
	}
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if s2.RendezvousCount() != 0 {
		t.Fatal("a restarted relay starts with no rendezvous")
	}
	if _, err := os.Stat(filepath.Join(f.s.DataDir(), "rendezvous", rvID)); !os.IsNotExist(err) {
		t.Fatalf("leftover blobs must be dropped at startup: %v", err)
	}
}

func TestRendezvousIDValidation(t *testing.T) {
	f := newRvFixture(t)
	for _, bad := range []string{"short", "has.dot.in.it", "with%20space", "toolong-" + string(bytes.Repeat([]byte("x"), 60))} {
		if code := f.put(t, bad, 1, []byte("x")); code != 400 {
			t.Fatalf("put with id %q must be 400, got %d", bad, code)
		}
		if code, _ := f.get(t, bad, 0, 0); code != 400 {
			t.Fatalf("get with id %q must be 400, got %d", bad, code)
		}
		if code := f.del(t, bad); code != 400 {
			t.Fatalf("delete with id %q must be 400, got %d", bad, code)
		}
	}
	if !ValidRendezvousID("abcdefgh") || !ValidRendezvousID("A-Z_09abcdefgh") {
		t.Fatal("8..64 chars of [A-Za-z0-9_-] must be valid")
	}
	// Bad bodies.
	post := func(body string) int {
		resp, err := http.Post(f.srv.URL+"/rendezvous/"+rvID, "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(`{"seq":0,"data":"eA=="}`); code != 400 {
		t.Fatalf("seq 0 must be 400, got %d", code)
	}
	if code := post(`{"seq":1,"data":"not base64!"}`); code != 400 {
		t.Fatalf("bad base64 must be 400, got %d", code)
	}
	if code := post(`not json`); code != 400 {
		t.Fatalf("bad json must be 400, got %d", code)
	}
}

func TestRendezvousSizeLimits(t *testing.T) {
	f := newRvFixture(t)
	// One blob over 4 MB is refused before anything is written.
	if code := f.put(t, rvID, 1, make([]byte, maxRendezvousBlob+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("blob over 4 MB must be 413, got %d", code)
	}
	if f.s.RendezvousCount() != 0 {
		t.Fatal("an oversized blob is refused before the rendezvous is even created")
	}
	if _, items := f.get(t, rvID, 0, 0); len(items) != 0 {
		t.Fatalf("refused blob must not be stored: %+v", items)
	}
	// Exactly 4 MB is fine.
	if code := f.put(t, rvID, 1, make([]byte, maxRendezvousBlob)); code != 200 {
		t.Fatalf("blob of exactly 4 MB must pass, got %d", code)
	}
	// The per-rendezvous total (lowered for the test) is enforced on decoded bytes.
	f.s.rvMaxTotal = 10 << 10
	other := "pair-code-total-cap1"
	if code := f.put(t, other, 1, make([]byte, 6<<10)); code != 200 {
		t.Fatalf("first 6 KB: %d", code)
	}
	if code := f.put(t, other, 2, make([]byte, 6<<10)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("second 6 KB must exceed the 10 KB cap with 413, got %d", code)
	}
	if code := f.put(t, other, 2, make([]byte, 4<<10)); code != 200 {
		t.Fatalf("4 KB still fits: %d", code)
	}
	if _, items := f.get(t, other, 0, 0); len(items) != 2 {
		t.Fatalf("expected the two accepted blobs: %d", len(items))
	}
}
