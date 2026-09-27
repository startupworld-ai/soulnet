// LAN rendezvous: the pairing rendezvous of the post office (relay/rendezvous.go), served
// by the sending device itself on the local network, so two devices on the same Wi-Fi
// exchange a join bundle directly instead of hauling it through a public relay.
//
// The wire protocol is exactly the relay's:
//
//	POST   /rendezvous/{id}   {seq, data}                 put one blob (data base64, <= a2a.RendezvousMaxBlob decoded)
//	GET    /rendezvous/{id}?since=<seq>&wait=<s>          {"items":[{seq,data}]} with seq > since, long-polls up to a2a.RendezvousMaxWait s
//	DELETE /rendezvous/{id}                               drop the rendezvous
//
// so the receiving device talks to it with a plain a2a.NewProxyClient("http://<lan-ip>:<port>", nil)
// and its RendezvousPut / RendezvousGet / RendezvousDelete -- no dedicated client. The
// sending device skips HTTP and calls Put / Get / Delete in process; their signatures match
// peer.Peer's, so a caller can swap one for the other.
//
// Security model:
//   - The service authenticates nobody. Confidentiality and integrity come from the caller:
//     every blob must already be sealed end-to-end with the pairing key, exactly as it is
//     for the public relay. A LAN eavesdropper sees what the relay would see: ciphertext.
//   - It only serves ids starting with a prefix the owner allowed (Allow / constructor):
//     the random id of the current pairing session. Any other path or id answers 404, so
//     nobody on the LAN can use it as a general-purpose drop box.
//   - Storage is memory only and bounded (per rendezvous a2a.RendezvousMaxTotal, overall
//     lanMaxTotal); idle rendezvous are dropped after rendezvousIdle.
//   - Callers open it for one pairing and Close it right after: Close stops listening,
//     releases every waiter and forgets every blob.
package peer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

const (
	// lanRendezvousIdle mirrors the relay: a rendezvous untouched this long is dropped.
	lanRendezvousIdle = 10 * time.Minute
	// lanMaxTotal caps the decoded bytes held across all rendezvous of one service (a
	// pairing uses a couple of ids; the allowed prefix must not become a memory sink).
	lanMaxTotal = 2 * a2a.RendezvousMaxTotal
	// lanMaxBody bounds the JSON request body (base64 inflates the blob by 4/3, plus framing).
	lanMaxBody = a2a.RendezvousMaxBlob/3*4 + 4096
)

// ErrLANRendezvousClosed is returned by the in-process calls after Close.
var ErrLANRendezvousClosed = errors.New("lan rendezvous: closed")

// LANRendezvous is an in-memory pairing rendezvous with an optional HTTP front end
// speaking the relay's /rendezvous/{id} protocol. The zero value is not usable; build it
// with NewLANRendezvous or ListenLAN. Safe for concurrent use.
type LANRendezvous struct {
	mu       sync.Mutex
	prefixes []string
	rv       map[string]*lanRV
	total    int64         // decoded bytes across all rendezvous
	wake     chan struct{} // closed (and replaced) on every put / delete / drop / close
	closed   bool
	waiting  atomic.Int32 // Get calls currently blocked (tests synchronise on it)

	// limits (fields so tests can shrink them)
	maxBlob     int
	maxTotal    int64 // per rendezvous
	maxAllTotal int64 // across the service
	idle        time.Duration

	ln  net.Listener
	srv *http.Server
}

// lanRV is one rendezvous.
type lanRV struct {
	last  time.Time
	size  int64
	blobs map[int64][]byte
}

// NewLANRendezvous returns an empty rendezvous store serving the ids that start with one of
// prefixes (more can be added with Allow). It does not listen; call Listen, or mount it
// as an http.Handler yourself.
func NewLANRendezvous(prefixes ...string) *LANRendezvous {
	l := &LANRendezvous{
		rv:          map[string]*lanRV{},
		wake:        make(chan struct{}),
		maxBlob:     a2a.RendezvousMaxBlob,
		maxTotal:    a2a.RendezvousMaxTotal,
		maxAllTotal: lanMaxTotal,
		idle:        lanRendezvousIdle,
	}
	for _, p := range prefixes {
		l.Allow(p)
	}
	return l
}

// ListenLAN starts a LAN rendezvous on 0.0.0.0:port (port 0 = a free port the OS picks)
// serving the ids that start with one of prefixes. Advertise URLs() to the other device.
func ListenLAN(port int, prefixes ...string) (*LANRendezvous, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("lan rendezvous: invalid port %d", port)
	}
	l := NewLANRendezvous(prefixes...)
	if err := l.Listen(net.JoinHostPort("0.0.0.0", strconv.Itoa(port))); err != nil {
		return nil, err
	}
	return l, nil
}

// Allow adds an id prefix to serve. An empty (or blank) prefix is ignored: it would open
// the service to every id.
func (l *LANRendezvous) Allow(prefix string) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.prefixes {
		if p == prefix {
			return
		}
	}
	l.prefixes = append(l.prefixes, prefix)
}

// allowed reports whether id falls under an allowed prefix. Caller holds mu.
func (l *LANRendezvous) allowedLocked(id string) bool {
	for _, p := range l.prefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// Listen serves the HTTP protocol on addr (host:port, TCP IPv4, e.g. "0.0.0.0:0" or
// "127.0.0.1:0") in the background until Close. At most one listener per service.
func (l *LANRendezvous) Listen(addr string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrLANRendezvousClosed
	}
	if l.ln != nil {
		return errors.New("lan rendezvous: already listening")
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("lan rendezvous: listen %s: %w", addr, err)
	}
	// No WriteTimeout: a GET long-polls up to RendezvousMaxWait and then streams a window
	// of up to RendezvousMaxTotal; the reader's own ctx bounds it.
	srv := &http.Server{Handler: l, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	l.ln, l.srv = ln, srv
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Addr is the listening address (nil before Listen).
func (l *LANRendezvous) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln == nil {
		return nil
	}
	return l.ln.Addr()
}

// Port is the listening TCP port (0 before Listen).
func (l *LANRendezvous) Port() int {
	if a, ok := l.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// URLs returns the candidate base URLs ("http://<ip>:<port>") the other device can try:
// one per private IPv4 address (10/8, 172.16/12, 192.168/16) on an up, non-loopback,
// non-virtual interface of this machine, then CGNAT / overlay addresses (100.64/10, e.g.
// Tailscale). Empty before Listen or when the machine has no such address.
func (l *LANRendezvous) URLs() []string {
	port := l.Port()
	if port == 0 {
		return nil
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var in []lanIface
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		in = append(in, lanIface{name: ifc.Name, flags: ifc.Flags, addrs: addrs})
	}
	var out []string
	for _, ip := range lanCandidateIPs(in) {
		out = append(out, "http://"+net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
	return out
}

// lanIface is the part of a net.Interface lanCandidateIPs looks at (testable without real NICs).
type lanIface struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

// virtualIfaceHints are substrings (lower case) of interface names that are virtual
// switches / container bridges: their addresses are unreachable from another device.
// Deliberately short -- a real NIC wrongly skipped only costs the LAN shortcut.
var virtualIfaceHints = []string{"docker", "vethernet", "veth", "vmware", "vmnet", "virtualbox", "vboxnet", "virbr", "br-", "hyper-v", "wsl"}

func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, h := range virtualIfaceHints {
		if strings.Contains(n, h) {
			return true
		}
	}
	return false
}

var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// lanCandidateIPs picks the advertisable IPv4 addresses: RFC 1918 first, CGNAT after,
// deduplicated, in interface order.
func lanCandidateIPs(ifaces []lanIface) []net.IP {
	var private, cgnat []net.IP
	seen := map[string]bool{}
	for _, ifc := range ifaces {
		if ifc.flags&net.FlagUp == 0 || ifc.flags&net.FlagLoopback != 0 || isVirtualIface(ifc.name) {
			continue
		}
		for _, a := range ifc.addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || seen[ip4.String()] {
				continue
			}
			switch {
			case ip4.IsPrivate():
				private = append(private, ip4)
			case cgnatNet.Contains(ip4):
				cgnat = append(cgnat, ip4)
			default:
				continue
			}
			seen[ip4.String()] = true
		}
	}
	return append(private, cgnat...)
}

// Close stops listening, releases every waiter and forgets every blob. Idempotent.
func (l *LANRendezvous) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.rv = map[string]*lanRV{}
	l.total = 0
	l.broadcastLocked()
	srv := l.srv
	l.mu.Unlock()
	if srv != nil {
		// Close (not Shutdown): long-polling GETs would otherwise hold it for up to 55 s.
		return srv.Close()
	}
	return nil
}

// broadcastLocked wakes every waiter so it re-checks its rendezvous. Caller holds mu.
func (l *LANRendezvous) broadcastLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}

// sweepLocked drops the rendezvous idle for longer than l.idle. Caller holds mu.
func (l *LANRendezvous) sweepLocked(now time.Time) {
	for id, st := range l.rv {
		if now.Sub(st.last) > l.idle {
			l.dropLocked(id)
		}
	}
}

// dropLocked removes one rendezvous and wakes the waiters. Caller holds mu.
func (l *LANRendezvous) dropLocked(id string) {
	if st, ok := l.rv[id]; ok {
		l.total -= st.size
		delete(l.rv, id)
		l.broadcastLocked()
	}
}

// relayErr builds the same error a2a.ProxyClient reports for the equivalent HTTP reply,
// so in-process callers branch on StatusCode exactly as they do against the relay.
func relayErr(status int, msg string) error { return &a2a.RelayError{StatusCode: status, Message: msg} }

// checkIDLocked validates id for any operation. Caller holds mu.
func (l *LANRendezvous) checkIDLocked(id string) error {
	if l.closed {
		return ErrLANRendezvousClosed
	}
	if !l.allowedLocked(id) {
		return relayErr(http.StatusNotFound, "not found")
	}
	if !a2a.ValidRendezvousID(id) {
		return relayErr(http.StatusBadRequest, "invalid rendezvous id")
	}
	return nil
}

// Put stores one blob (already sealed by the caller) at rendezvous id under seq. Errors
// mirror the relay: 400 bad id / seq, 404 id not allowed, 409 seq already stored, 413 blob
// or rendezvous too large (as *a2a.RelayError); ErrLANRendezvousClosed after Close.
func (l *LANRendezvous) Put(ctx context.Context, id string, seq int64, data []byte) error {
	if err := ctxOrBackground(ctx).Err(); err != nil {
		return err
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkIDLocked(id); err != nil {
		return err
	}
	if seq <= 0 {
		return relayErr(http.StatusBadRequest, "seq must be a positive integer")
	}
	if len(data) > l.maxBlob {
		return relayErr(http.StatusRequestEntityTooLarge, "blob exceeds 4 MB")
	}
	l.sweepLocked(now)
	st := l.rv[id]
	if st != nil {
		st.last = now
		if _, dup := st.blobs[seq]; dup {
			return relayErr(http.StatusConflict, "seq already stored")
		}
	}
	size := int64(len(data))
	var cur int64
	if st != nil {
		cur = st.size
	}
	if cur+size > l.maxTotal {
		return relayErr(http.StatusRequestEntityTooLarge, "rendezvous exceeds 64 MB")
	}
	if l.total+size > l.maxAllTotal {
		return relayErr(http.StatusRequestEntityTooLarge, "lan rendezvous storage is full")
	}
	if st == nil {
		st = &lanRV{last: now, blobs: map[int64][]byte{}}
		l.rv[id] = st
	}
	st.blobs[seq] = append([]byte(nil), data...)
	st.size += size
	l.total += size
	l.broadcastLocked()
	return nil
}

// readLocked returns copies of the blobs of id with seq > since in seq order, whether the
// rendezvous exists, and the channel to wait on for a change. Caller holds mu.
func (l *LANRendezvous) readLocked(id string, since int64, now time.Time) ([]a2a.RendezvousItem, bool, <-chan struct{}) {
	l.sweepLocked(now)
	st := l.rv[id]
	if st == nil {
		return nil, false, l.wake
	}
	st.last = now
	seqs := make([]int64, 0, len(st.blobs))
	for seq := range st.blobs {
		if seq > since {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	items := make([]a2a.RendezvousItem, 0, len(seqs))
	for _, seq := range seqs {
		items = append(items, a2a.RendezvousItem{Seq: seq, Data: append([]byte(nil), st.blobs[seq]...)})
	}
	return items, true, l.wake
}

// Get returns the blobs at id with seq > since, in seq order. waitSec > 0 waits (capped
// at a2a.RendezvousMaxWait) until at least one such blob exists; an unknown rendezvous
// is not an error (the reader may arrive first) and yields an empty list once the wait
// ends. A wait also ends early, with an empty list, when the rendezvous is deleted; Close
// ends it with ErrLANRendezvousClosed and ctx cancellation with ctx.Err().
func (l *LANRendezvous) Get(ctx context.Context, id string, since int64, waitSec int) ([]a2a.RendezvousItem, error) {
	ctx = ctxOrBackground(ctx)
	wait := min(max(waitSec, 0), a2a.RendezvousMaxWait)
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(time.Duration(wait) * time.Second)
		defer t.Stop()
		timeout = t.C
	}
	existed := false
	for {
		l.mu.Lock()
		if err := l.checkIDLocked(id); err != nil {
			l.mu.Unlock()
			return nil, err // includes ErrLANRendezvousClosed for a waiter released by Close
		}
		items, exists, wake := l.readLocked(id, since, time.Now())
		l.mu.Unlock()
		if len(items) > 0 || wait == 0 || (existed && !exists) {
			if items == nil {
				items = []a2a.RendezvousItem{}
			}
			return items, nil
		}
		existed = existed || exists
		l.waiting.Add(1)
		select {
		case <-wake:
			l.waiting.Add(-1)
		case <-timeout:
			l.waiting.Add(-1)
			return []a2a.RendezvousItem{}, nil
		case <-ctx.Done():
			l.waiting.Add(-1)
			return nil, ctx.Err()
		}
	}
}

// Delete drops rendezvous id and its blobs (idempotent) and releases its waiters.
func (l *LANRendezvous) Delete(ctx context.Context, id string) error {
	if err := ctxOrBackground(ctx).Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkIDLocked(id); err != nil {
		return err
	}
	l.dropLocked(id)
	return nil
}

// ServeHTTP implements the relay's /rendezvous/{id} protocol (and 404 for everything else).
func (l *LANRendezvous) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/rendezvous/")
	if !ok || id == "" || strings.Contains(id, "/") {
		lanWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	l.mu.Lock()
	allowed := l.allowedLocked(id)
	l.mu.Unlock()
	if !allowed {
		lanWriteErr(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodPost:
		l.httpPut(w, r, id)
	case http.MethodGet:
		l.httpGet(w, r, id)
	case http.MethodDelete:
		l.writeResult(w, l.Delete(r.Context(), id), map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		lanWriteErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (l *LANRendezvous) httpPut(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Seq  int64  `json:"seq"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, lanMaxBody)).Decode(&body); err != nil {
		lanWriteErr(w, http.StatusBadRequest, "request body must be {seq, data} (data base64, at most 4 MB decoded)")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		lanWriteErr(w, http.StatusBadRequest, "data must be base64")
		return
	}
	l.writeResult(w, l.Put(r.Context(), id, body.Seq, raw), map[string]any{"ok": true, "seq": body.Seq})
}

func (l *LANRendezvous) httpGet(w http.ResponseWriter, r *http.Request, id string) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	items, err := l.Get(r.Context(), id, since, wait)
	if err != nil && r.Context().Err() != nil {
		return // the reader went away
	}
	l.writeResult(w, err, map[string]any{"items": items})
}

// writeResult answers ok with 200 or maps err to the relay's status + {"error": msg}.
func (l *LANRendezvous) writeResult(w http.ResponseWriter, err error, ok any) {
	if err == nil {
		lanWriteJSON(w, http.StatusOK, ok)
		return
	}
	var re *a2a.RelayError
	switch {
	case errors.As(err, &re):
		lanWriteErr(w, re.StatusCode, re.Message)
	case errors.Is(err, ErrLANRendezvousClosed):
		lanWriteErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		lanWriteErr(w, http.StatusInternalServerError, err.Error())
	}
}

func lanWriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func lanWriteErr(w http.ResponseWriter, code int, msg string) {
	lanWriteJSON(w, code, map[string]string{"error": msg})
}
