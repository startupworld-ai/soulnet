// Package relay implements the soulnet mail relay: the A2A post office (A2A wire spec §7) plus the opt-in
// capability directory (§8).
//
// The post office does exactly three things: accept encrypted, signed envelopes; stage them in buckets keyed by the
// recipient public-key fingerprint; delete them once fetched. A pure dumb pipe -- it cannot read the
// content (E2E encrypted), does not know who knows whom (no friend list), and stores no accounts
// (identity is the key pair, no registration). Anyone can self-host: one official, one per company intranet, one per
// personal VPS. File storage, single-binary deployment.
//
// Everything else a deployment may want on top of the mailbox (tunnels, app markets, ledgers, feedback boards ...)
// is NOT part of this package: products mount their own handlers through the small extension API in ext.go
// (Handle / Use / Subscribe / AdminOK / VerifyRequest / DataDir). The core never imports them.
package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// Server is the post office HTTP service. All state lives in files under dataDir.
type Server struct {
	dataDir string

	wkMu   sync.Mutex
	wakeup map[string]chan struct{} // long-poll wakeup: mailbox (fingerprint) -> new-mail signal

	rlMu     sync.Mutex
	rlWindow map[string]*rateWindow // rate limiting keyed by sender public key

	seenMu   sync.Mutex
	lastSeen map[string]time.Time // mailbox (fingerprint) -> last long-poll time (online detection)

	// grMu serializes group roster read-modify-write (publish version checks; see group.go).
	grMu sync.Mutex

	// Capability directory: isolated from the dumb-pipe logic.
	dir *Directory

	// adminToken is the shared secret behind AdminOK / RequireAdmin (used by extensions, the core has no admin routes).
	// Empty = admin checks always fail (secure default: nothing is exposed unless a token is set explicitly).
	adminToken string

	// Extension plumbing (see ext.go): the route table extensions mount into, the outer middlewares and the event subscribers.
	mux    *http.ServeMux
	rtMu   sync.Mutex
	routes map[string]bool
	wraps  []func(http.Handler) http.Handler

	evMu   sync.RWMutex
	subs   map[uint64]func(Event)
	subSeq uint64
}

// SetAdminToken sets the admin token checked by AdminOK / RequireAdmin (empty => every admin check fails).
func (s *Server) SetAdminToken(t string) { s.adminToken = strings.TrimSpace(t) }

// New creates the post office service and makes sure the data directory is ready.
func New(dataDir string) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "inbox"), 0o755); err != nil {
		return nil, err
	}
	s := &Server{
		dataDir:  dataDir,
		wakeup:   map[string]chan struct{}{},
		rlWindow: map[string]*rateWindow{},
		lastSeen: map[string]time.Time{},
		dir:      NewDirectory(dataDir),
		mux:      http.NewServeMux(),
		routes:   map[string]bool{},
		subs:     map[uint64]func(Event){},
	}
	s.dir.afterPublish = func(fp string) { s.emit(Event{Kind: EventDirectoryPublished, FP: fp}) }
	s.mountCore()
	return s, nil
}

// Directory returns the capability directory (read access for extensions and tests).
func (s *Server) Directory() *Directory { return s.dir }

func (s *Server) inboxDir(fp string) string { return filepath.Join(s.dataDir, "inbox", fp) }

// SafeBox reports whether a mailbox name (fingerprint) is safe to use as a directory name.
func SafeBox(fp string) bool {
	return fp != "" && !strings.ContainsAny(fp, `/\`) && !strings.Contains(fp, "..")
}

// deliverSeq is the delivery sequence: monotonically increasing within the process, giving letters delivered "within the same nanosecond tick" a unique, ordered number.
var deliverSeq atomic.Uint64

// inboxFileName builds an inbox file name that guarantees "unique + lexical order == delivery order".
//
// Why a nanosecond timestamp alone is not enough (historical bug): the Windows wall clock has a granularity of ~0.5ms -- measured
// locally, 200k consecutive time.Now().UnixNano() calls returned 199999 values identical to the previous one, smallest non-zero gap 504.7us.
// Two letters delivered to the same mailbox within half a millisecond got the same name, os.WriteFile silently overwrote the first,
// and the letter vanished without any error (measured: 500 rapid deliveries left only 174 files). Symptom: A2A mission state stuck.
//
// Why not O_CREATE|O_EXCL with retry on conflict: under a coarse clock hundreds or thousands of letters can pile up in one tick;
// retrying would probe every taken name one by one, degrading to O(n^2) syscalls, and concurrent goroutines keep
// colliding on the same candidate. An atomic counter gets it right in one shot -- no retry, no lock, no failure path, fully deterministic.
//
// Why seq must be fixed-width zero-padded: readInbox restores delivery order by lexical file-name order (see its sort.Strings).
// Written as <nano>-10 / <nano>-9, lexical order gives "10" < "9" and the order flips; with fixed 12 digits lexical order == numeric order.
// The nanosecond part stays 19 digits until the year 2262, so lexical order == numeric order there too; and '-'(0x2D) < '0'(0x30),
// so legacy bare <nano>.json files still sort correctly before new files of "the same nanosecond" -- no migration needed.
//
// The timestamp remains the primary key (rather than the counter alone): the counter resets on restart, and only the
// timestamp keeps files ordered by real time across restarts.
func inboxFileName() string {
	return fmt.Sprintf("%019d-%012d.json", time.Now().UnixNano(), deliverSeq.Add(1))
}

// deliver writes the envelope into the inbox and wakes up its long-poller.
func (s *Server) deliver(env *a2a.Envelope) error {
	dir := s.inboxDir(env.To)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.Marshal(env)
	name := inboxFileName()
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		log.Printf("[relay-debug] deliver FAIL to=%s gid=%s file=%s err=%v", a2a.ShortFp(env.To), a2a.ShortFp(env.GID), name, err)
		return err
	}
	log.Printf("[relay-debug] deliver OK to=%s gid=%s file=%s bytes=%d", a2a.ShortFp(env.To), a2a.ShortFp(env.GID), name, len(raw))
	s.wkMu.Lock()
	if ch, ok := s.wakeup[env.To]; ok {
		close(ch)
		delete(s.wakeup, env.To)
	}
	s.wkMu.Unlock()
	return nil
}

// --- rate limiting (abuse protection) ---

type rateWindow struct {
	start time.Time
	count int
}

func (s *Server) rateLimit(key string) bool {
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	w := s.rlWindow[key]
	now := time.Now()
	if w == nil || now.Sub(w.start) > time.Minute {
		s.rlWindow[key] = &rateWindow{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= 240
}

// --- HTTP ---

// mountCore registers the core routes (mailbox + presence + health + capability directory) on the route table.
func (s *Server) mountCore() {
	must := func(err error) {
		if err != nil {
			panic(err) // core routes are registered exactly once on a fresh mux; a conflict here is a programming error
		}
	}
	must(s.HandleFunc("POST /mail", s.postMail))
	must(s.HandleFunc("GET /mail", s.getMail))
	must(s.HandleFunc("POST /mail/ack", s.ackMail))
	must(s.HandleFunc("GET /presence", s.presence))
	must(s.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]any{"ok": true, "service": "soulnet-relay", "v": 2})
	}))
	s.mountGroups(must)
	must(s.HandleFunc("POST /directory/publish", s.dir.handlePublish))
	must(s.HandleFunc("POST /directory/unpublish", s.dir.handleUnpublish))
	must(s.HandleFunc("GET /directory/query", s.dir.handleQuery))
	must(s.HandleFunc("GET /directory/fetch", s.dir.handleFetch))
}

// Handler returns the http.Handler with all routes mounted: core routes plus whatever extensions registered through
// Handle / HandleFunc, wrapped by the middlewares installed with Use (outermost = first installed).
func (s *Server) Handler() http.Handler {
	s.rtMu.Lock()
	wraps := append([]func(http.Handler) http.Handler(nil), s.wraps...)
	s.rtMu.Unlock()
	var h http.Handler = s.mux
	for i := len(wraps) - 1; i >= 0; i-- {
		h = wraps[i](h)
	}
	return h
}

func (s *Server) markSeen(box string) {
	s.seenMu.Lock()
	s.lastSeen[box] = time.Now()
	s.seenMu.Unlock()
}

// presence: GET /presence?box=<fingerprint> -> {online}. The mailbox owner counts as online if it long-polled within the last 75s.
// No auth required: it only exposes "is this fingerprint currently connected to the post office", never content (ciphertext stays ciphertext).
func (s *Server) presence(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	s.seenMu.Lock()
	last, ok := s.lastSeen[box]
	s.seenMu.Unlock()
	WriteJSON(w, 200, map[string]any{"online": ok && time.Since(last) < 75*time.Second})
}

// WriteJSON writes v as a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard {"error": msg} JSON error body with the given status code.
func WriteError(w http.ResponseWriter, code int, msg string) {
	WriteJSON(w, code, map[string]string{"error": msg})
}

// postMail: deliver one letter. No auth -- the envelope carries its own signature; the post office verifies it is not forged and accepts.
// Anyone can drop a validly signed letter into any mailbox (like postal mail); abuse is handled by sender rate limits + recipient allowlists.
func (s *Server) postMail(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB per letter
	if err != nil {
		WriteError(w, 400, "failed to read request body")
		return
	}
	var env a2a.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		WriteError(w, 400, "invalid envelope JSON")
		return
	}
	if !SafeBox(env.To) {
		WriteError(w, 400, "invalid recipient address")
		return
	}
	if err := env.VerifyEnvelope(); err != nil {
		WriteError(w, 401, "envelope signature check failed: "+err.Error())
		return
	}
	if !s.rateLimit(env.From) {
		WriteError(w, 429, "sending too frequently")
		return
	}
	if err := s.deliver(&env); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	s.emit(Event{Kind: EventMailDelivered, FP: env.To, Data: map[string]any{"from": env.From}})
	WriteJSON(w, 200, map[string]any{"ok": true})
}

// authBox checks inbox-side requests: prove ownership of mailbox box (signing key fingerprint == box).
func (s *Server) authBox(r *http.Request, method, path, box string) error {
	fp, err := VerifyRequest(r, method, path)
	if err != nil {
		return err
	}
	if fp != box {
		return errors.New("not allowed to read this mailbox")
	}
	return nil
}

// getMail: fetch pending letters for this mailbox (GET /mail?box=<fingerprint>&wait=25). wait>0 long-polls.
func (s *Server) getMail(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "GET", "/mail", box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	s.markSeen(box) // the owner of this mailbox is online right now (long-polling)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait > 55 {
		wait = 55
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		items, err := s.readInbox(box)
		if err != nil {
			WriteError(w, 500, err.Error())
			return
		}
		if len(items) > 0 || wait <= 0 || time.Now().After(deadline) {
			log.Printf("[relay-debug] getMail box=%s wait=%d items=%d", a2a.ShortFp(box), wait, len(items))
			WriteJSON(w, 200, map[string]any{"messages": items})
			return
		}
		s.wkMu.Lock()
		ch, ok := s.wakeup[box]
		if !ok {
			ch = make(chan struct{})
			s.wakeup[box] = ch
		}
		s.wkMu.Unlock()
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		case <-r.Context().Done():
			return
		}
	}
}

type mailItem struct {
	a2a.Envelope
	AckID string `json:"ack_id"`
}

func (s *Server) readInbox(box string) ([]mailItem, error) {
	dir := s.inboxDir(box)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []mailItem{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // file names are fixed-width "nano-seq" (see inboxFileName) -> lexical order is delivery order
	items := make([]mailItem, 0, len(names))
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		var env a2a.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		items = append(items, mailItem{Envelope: env, AckID: strings.TrimSuffix(n, ".json")})
	}
	return items, nil
}

// ackMail: acknowledge receipt, the post office deletes the letters (POST /mail/ack {box, ack_ids}).
func (s *Server) ackMail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Box    string   `json:"box"`
		AckIDs []string `json:"ack_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {box, ack_ids}")
		return
	}
	if !SafeBox(body.Box) {
		WriteError(w, 400, "invalid box")
		return
	}
	if err := s.authBox(r, "POST", "/mail/ack", body.Box); err != nil {
		WriteError(w, 401, err.Error())
		return
	}
	dir := s.inboxDir(body.Box)
	n := 0
	for _, id := range body.AckIDs {
		if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
			continue
		}
		if os.Remove(filepath.Join(dir, id+".json")) == nil {
			n++
		}
	}
	s.emit(Event{Kind: EventMailAcked, FP: body.Box, Data: map[string]any{"removed": n}})
	log.Printf("[relay-debug] ackMail box=%s requested=%d removed=%d", a2a.ShortFp(body.Box), len(body.AckIDs), n)
	WriteJSON(w, 200, map[string]any{"ok": true, "removed": n})
}
