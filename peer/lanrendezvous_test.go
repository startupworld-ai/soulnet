package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

const lanPrefix = "sess-abcdef01"
const lanID = lanPrefix + "-main"

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var re *a2a.RelayError
	if !errors.As(err, &re) || re.StatusCode != status {
		t.Fatalf("want relay status %d, got %v", status, err)
	}
}

// waitForWaiters spins until n Get calls are blocked (no fixed sleeps).
func waitForWaiters(t *testing.T, l *LANRendezvous, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.waiting.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d blocked Get calls, have %d", n, l.waiting.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

func seqsOf(items []a2a.RendezvousItem) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.Seq)
	}
	return out
}

func TestLANRendezvousStoreSemantics(t *testing.T) {
	l := NewLANRendezvous(lanPrefix)
	defer l.Close()
	ctx := context.Background()

	// Unknown (but allowed) id: empty list, not an error.
	if items, err := l.Get(ctx, lanID, 0, 0); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("unknown id must read as an empty list: %v %v", items, err)
	}
	// Out-of-order puts come back in seq order; since filters.
	for _, seq := range []int64{2, 1, 3} {
		if err := l.Put(ctx, lanID, seq, []byte(fmt.Sprintf("blob-%d", seq))); err != nil {
			t.Fatal(err)
		}
	}
	items, err := l.Get(ctx, lanID, 0, 0)
	if err != nil || fmt.Sprint(seqsOf(items)) != "[1 2 3]" || string(items[0].Data) != "blob-1" {
		t.Fatalf("order: %v %v", seqsOf(items), err)
	}
	items[0].Data[0] = 'X' // callers get copies
	if items, _ := l.Get(ctx, lanID, 1, 0); fmt.Sprint(seqsOf(items)) != "[2 3]" {
		t.Fatalf("since: %v", seqsOf(items))
	}
	if items, _ := l.Get(ctx, lanID, 0, 0); string(items[0].Data) != "blob-1" {
		t.Fatal("stored blob was modified through a returned item")
	}
	// Duplicate seq, bad seq, bad id, id outside the allowed prefix.
	wantStatus(t, l.Put(ctx, lanID, 2, []byte("again")), http.StatusConflict)
	wantStatus(t, l.Put(ctx, lanID, 0, []byte("x")), http.StatusBadRequest)
	wantStatus(t, l.Put(ctx, lanPrefix+"/../x", 1, []byte("x")), http.StatusBadRequest)
	wantStatus(t, l.Put(ctx, "other-session-id", 1, []byte("x")), http.StatusNotFound)
	_, err = l.Get(ctx, "other-session-id", 0, 0)
	wantStatus(t, err, http.StatusNotFound)
	wantStatus(t, l.Delete(ctx, "other-session-id"), http.StatusNotFound)

	// Delete frees the storage and reads empty afterwards.
	if err := l.Delete(ctx, lanID); err != nil {
		t.Fatal(err)
	}
	if items, err := l.Get(ctx, lanID, 0, 0); err != nil || len(items) != 0 {
		t.Fatalf("deleted rendezvous must read empty: %v %v", items, err)
	}
	if err := l.Delete(ctx, lanID); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if l.total != 0 {
		t.Fatalf("storage not released: %d", l.total)
	}
}

func TestLANRendezvousLimits(t *testing.T) {
	l := NewLANRendezvous(lanPrefix)
	defer l.Close()
	l.maxBlob, l.maxTotal, l.maxAllTotal = 10, 25, 40
	ctx := context.Background()

	wantStatus(t, l.Put(ctx, lanID, 1, make([]byte, 11)), http.StatusRequestEntityTooLarge)
	for seq := int64(1); seq <= 2; seq++ {
		if err := l.Put(ctx, lanID, seq, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
	}
	// Per rendezvous: 20 + 10 > 25.
	wantStatus(t, l.Put(ctx, lanID, 3, make([]byte, 10)), http.StatusRequestEntityTooLarge)
	// Across the service: 20 + 10 + 10 = 40 fits, one more byte does not.
	other := lanPrefix + "-ack"
	if err := l.Put(ctx, other, 1, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := l.Put(ctx, other, 2, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, l.Put(ctx, other, 3, make([]byte, 1)), http.StatusRequestEntityTooLarge)
	// Deleting one rendezvous makes room again.
	if err := l.Delete(ctx, lanID); err != nil {
		t.Fatal(err)
	}
	if err := l.Put(ctx, lanID, 1, make([]byte, 10)); err != nil {
		t.Fatalf("room must be released by Delete: %v", err)
	}
	// Idle rendezvous are dropped lazily.
	l.idle = 0
	time.Sleep(2 * time.Millisecond)
	if items, _ := l.Get(ctx, other, 0, 0); len(items) != 0 {
		t.Fatalf("idle rendezvous must be dropped: %v", seqsOf(items))
	}
}

func TestLANRendezvousLongPollWakeups(t *testing.T) {
	l := NewLANRendezvous(lanPrefix)
	defer l.Close()
	ctx := context.Background()

	type res struct {
		items []a2a.RendezvousItem
		err   error
	}
	// A reader that arrives before the writer is woken by the first Put.
	done := make(chan res, 1)
	go func() {
		items, err := l.Get(ctx, lanID, 0, 30)
		done <- res{items, err}
	}()
	waitForWaiters(t, l, 1)
	if err := l.Put(ctx, lanID, 1, []byte("chunk-1")); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || fmt.Sprint(seqsOf(r.items)) != "[1]" {
		t.Fatalf("Put must wake the reader: %v %v", seqsOf(r.items), r.err)
	}

	// A reader waiting past the last seq is released (empty) when the rendezvous is deleted.
	go func() {
		items, err := l.Get(ctx, lanID, 1, 30)
		done <- res{items, err}
	}()
	waitForWaiters(t, l, 1)
	if err := l.Delete(ctx, lanID); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || len(r.items) != 0 {
		t.Fatalf("Delete must release the reader with an empty list: %v %v", seqsOf(r.items), r.err)
	}

	// ctx cancellation ends a wait.
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		items, err := l.Get(cctx, lanID, 0, 30)
		done <- res{items, err}
	}()
	waitForWaiters(t, l, 1)
	cancel()
	if r := <-done; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ctx cancel must end the wait: %v", r.err)
	}

	// Close releases waiters with ErrLANRendezvousClosed and refuses further calls.
	go func() {
		items, err := l.Get(ctx, lanID, 0, 30)
		done <- res{items, err}
	}()
	waitForWaiters(t, l, 1)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if r := <-done; !errors.Is(r.err, ErrLANRendezvousClosed) {
		t.Fatalf("Close must release the reader: %v", r.err)
	}
	if err := l.Put(ctx, lanID, 1, []byte("x")); !errors.Is(err, ErrLANRendezvousClosed) {
		t.Fatalf("Put after Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

// The HTTP front end speaks the relay protocol: the stock a2a.ProxyClient works against it.
func TestLANRendezvousHTTPWithProxyClient(t *testing.T) {
	l := NewLANRendezvous()
	l.Allow(lanPrefix)
	l.Allow("") // ignored: must not open every id
	if err := l.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := "http://" + l.Addr().String()
	pc := a2a.NewProxyClient(base, nil)
	ctx := context.Background()

	big := bytes.Repeat([]byte{0xA5}, a2a.RendezvousMaxBlob) // a full-size blob fits the body limit
	if err := pc.RendezvousPut(ctx, lanID, 2, big); err != nil {
		t.Fatal(err)
	}
	if err := pc.RendezvousPut(ctx, lanID, 1, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	items, err := pc.RendezvousGet(ctx, lanID, 0, 0)
	if err != nil || fmt.Sprint(seqsOf(items)) != "[1 2]" || string(items[0].Data) != "hello" || !bytes.Equal(items[1].Data, big) {
		t.Fatalf("http round trip: %v %v", seqsOf(items), err)
	}
	wantStatus(t, pc.RendezvousPut(ctx, lanID, 1, []byte("dup")), http.StatusConflict)
	wantStatus(t, pc.RendezvousPut(ctx, lanID, 3, append(big, 0)), http.StatusRequestEntityTooLarge)

	// Long poll over HTTP, woken by the sender's in-process Put.
	type res struct {
		items []a2a.RendezvousItem
		err   error
	}
	done := make(chan res, 1)
	go func() {
		items, err := pc.RendezvousGet(ctx, lanID, 2, 30)
		done <- res{items, err}
	}()
	waitForWaiters(t, l, 1)
	if err := l.Put(ctx, lanID, 3, []byte("three")); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || fmt.Sprint(seqsOf(r.items)) != "[3]" {
		t.Fatalf("http long poll: %v %v", seqsOf(r.items), r.err)
	}

	// Ids outside the allowed prefix, and every other path, are 404.
	wantStatus(t, pc.RendezvousPut(ctx, "someone-elses-id", 1, []byte("x")), http.StatusNotFound)
	_, err = pc.RendezvousGet(ctx, "someone-elses-id", 0, 0)
	wantStatus(t, err, http.StatusNotFound)
	wantStatus(t, pc.RendezvousDelete(ctx, "someone-elses-id"), http.StatusNotFound)
	for _, path := range []string{"/", "/mail", "/rendezvous/", "/rendezvous/" + lanID + "/x"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", path, resp.StatusCode)
		}
	}

	if err := pc.RendezvousDelete(ctx, lanID); err != nil {
		t.Fatal(err)
	}
	if items, err := pc.RendezvousGet(ctx, lanID, 0, 0); err != nil || len(items) != 0 {
		t.Fatalf("deleted over http: %v %v", seqsOf(items), err)
	}

	// Close stops the listener.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pc.RendezvousPut(ctx, lanID, 1, []byte("x")); err == nil {
		t.Fatal("closed service must not accept connections")
	}
}

// A GET asking for a2a.RendezvousItemsBinary gets binary frames; a plain GET (an older
// client) still gets the relay's JSON; errors stay JSON either way.
func TestLANRendezvousServesBinaryOnRequest(t *testing.T) {
	l := NewLANRendezvous(lanPrefix)
	if err := l.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := "http://" + l.Addr().String()
	ctx := context.Background()
	big := bytes.Repeat([]byte{0x5A}, 300000)
	if err := l.Put(ctx, lanID, 1, big); err != nil {
		t.Fatal(err)
	}
	if err := l.Put(ctx, lanID, 2, []byte("two")); err != nil {
		t.Fatal(err)
	}
	get := func(id, accept string) *http.Response {
		req, _ := http.NewRequest("GET", base+"/rendezvous/"+id+"?since=0&wait=0", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get(lanID, a2a.RendezvousItemsBinary)
	items, err := a2a.ReadRendezvousItemsBinary(resp.Body)
	resp.Body.Close()
	if !a2a.IsRendezvousBinary(resp.Header.Get("Content-Type")) || err != nil || fmt.Sprint(seqsOf(items)) != "[1 2]" || !bytes.Equal(items[0].Data, big) {
		t.Fatalf("binary reply: ct=%q %v %v", resp.Header.Get("Content-Type"), seqsOf(items), err)
	}
	if resp.ContentLength != int64(a2a.RendezvousItemsBinarySize(items)) {
		t.Fatalf("Content-Length %d", resp.ContentLength)
	}

	resp = get(lanID, "")
	var out struct {
		Items []a2a.RendezvousItem `json:"items"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") || err != nil || len(out.Items) != 2 || !bytes.Equal(out.Items[0].Data, big) {
		t.Fatalf("json reply for a plain GET: ct=%q %v", resp.Header.Get("Content-Type"), err)
	}

	resp = get("someone-elses-id", a2a.RendezvousItemsBinary)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("errors stay JSON: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// And the stock client gets the binary form transparently.
	got, err := a2a.NewProxyClient(base, nil).RendezvousGet(ctx, lanID, 1, 0)
	if err != nil || fmt.Sprint(seqsOf(got)) != "[2]" || string(got[0].Data) != "two" {
		t.Fatalf("proxy client: %v %v", seqsOf(got), err)
	}
}

func TestLANRendezvousURLs(t *testing.T) {
	l := NewLANRendezvous(lanPrefix)
	if l.URLs() != nil {
		t.Fatal("no URLs before Listen")
	}
	if err := l.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Logf("candidates: %v", l.URLs())
	suffix := fmt.Sprintf(":%d", l.Port())
	for _, u := range l.URLs() {
		if strings.Contains(u, "127.0.0.1") || !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, suffix) {
			t.Fatalf("bad candidate URL %q", u)
		}
	}
}

func TestLANCandidateIPs(t *testing.T) {
	ipn := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	up := net.FlagUp | net.FlagBroadcast
	got := lanCandidateIPs([]lanIface{
		{name: "lo", flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{ipn("127.0.0.1/8"), ipn("10.9.9.9/8")}},
		{name: "tailscale0", flags: up, addrs: []net.Addr{ipn("100.101.102.103/32")}},
		{name: "Wi-Fi", flags: up, addrs: []net.Addr{ipn("fe80::1/64"), ipn("192.168.1.20/24"), ipn("169.254.3.4/16")}},
		{name: "eth1", flags: 0, addrs: []net.Addr{ipn("192.168.7.7/24")}}, // down
		{name: "docker0", flags: up, addrs: []net.Addr{ipn("172.17.0.1/16")}},
		{name: "vEthernet (WSL)", flags: up, addrs: []net.Addr{ipn("172.20.0.1/20")}},
		{name: "VMware Network Adapter VMnet8", flags: up, addrs: []net.Addr{ipn("192.168.80.1/24")}},
		{name: "en0", flags: up, addrs: []net.Addr{ipn("8.8.8.8/24"), ipn("10.0.0.5/8"), ipn("172.16.4.4/12"), ipn("192.168.1.20/24")}},
	})
	want := "[192.168.1.20 10.0.0.5 172.16.4.4 100.101.102.103]"
	if fmt.Sprint(got) != want {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}
