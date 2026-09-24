// Extension API of the relay core.
//
// The core is deliberately small (mailbox + capability directory). Products that need more -- reverse tunnels,
// app markets, ledgers, feedback boards, remote-shell rendezvous -- mount their own handlers on the same listener
// through the handful of hooks below instead of forking the mailbox. The contract is intentionally tiny:
//
//   - Handle / HandleFunc   add a route to the core mux (same pattern syntax as net/http.ServeMux; duplicates error)
//   - Use                   wrap the whole handler with a middleware (e.g. route by Host before the mux sees the request)
//   - DataDir               where the core keeps its files; extensions put theirs under the same root
//   - AdminOK / RequireAdmin the shared admin-token check (token set with SetAdminToken)
//   - VerifyRequest         the A2A request-signature check (method+path+timestamp signed by the caller's key)
//   - Subscribe             observe core events (directory.published, mail.delivered, mail.acked, box.active_changed, ...)
//
// Register routes and middlewares before the handler starts serving; event subscriptions may come and go at any time.
package relay

import (
	"crypto/subtle"
	"fmt"
	"net/http"

	"github.com/startupworld-ai/soulnet/a2a"
)

// Event kinds emitted by the core. Extensions subscribe with (*Server).Subscribe.
const (
	EventDirectoryPublished = "directory.published" // FP = fingerprint whose capability card was (re)listed
	EventMailDelivered      = "mail.delivered"      // FP = recipient mailbox; Data["from"] = sender public key (base64)
	EventMailAcked          = "mail.acked"          // FP = mailbox that acknowledged; Data["removed"] = number of letters deleted
)

// Event is one core notification. Kind is one of the Event* constants, FP the fingerprint the event is about,
// Data optional kind-specific details. Events are emitted only after the underlying action succeeded.
type Event struct {
	Kind string
	FP   string
	Data map[string]any
}

// Subscribe registers fn for every core event and returns a cancel function that removes the subscription.
// fn runs synchronously on the request goroutine that produced the event -- keep it fast and never block;
// a panic inside fn is recovered so a misbehaving subscriber cannot take the request down.
func (s *Server) Subscribe(fn func(Event)) (cancel func()) {
	s.evMu.Lock()
	s.subSeq++
	id := s.subSeq
	s.subs[id] = fn
	s.evMu.Unlock()
	return func() {
		s.evMu.Lock()
		delete(s.subs, id)
		s.evMu.Unlock()
	}
}

// emit fans one event out to every subscriber (best-effort, panics recovered).
func (s *Server) emit(ev Event) {
	s.evMu.RLock()
	fns := make([]func(Event), 0, len(s.subs))
	for _, fn := range s.subs {
		fns = append(fns, fn)
	}
	s.evMu.RUnlock()
	for _, fn := range fns {
		func() {
			defer func() { _ = recover() }()
			fn(ev)
		}()
	}
}

// DataDir returns the data directory the core was created with. Extensions keep their own files beneath it
// (each in its own sub-directory; the core owns inbox/ and directory/).
func (s *Server) DataDir() string { return s.dataDir }

// Handle mounts h on the core mux under pattern (net/http.ServeMux syntax, e.g. "POST /api/app/publish").
// Registering the same pattern twice -- or a pattern the core already owns -- returns an error instead of panicking.
func (s *Server) Handle(pattern string, h http.Handler) (err error) {
	s.rtMu.Lock()
	defer s.rtMu.Unlock()
	if s.routes[pattern] {
		return fmt.Errorf("relay: route %q already registered", pattern)
	}
	defer func() {
		if r := recover(); r != nil { // ServeMux panics on conflicting patterns; surface it as an error
			err = fmt.Errorf("relay: route %q conflicts: %v", pattern, r)
		}
	}()
	s.mux.Handle(pattern, h)
	s.routes[pattern] = true
	return nil
}

// HandleFunc is Handle for a plain handler function.
func (s *Server) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) error {
	return s.Handle(pattern, http.HandlerFunc(f))
}

// Use installs a middleware around the whole handler returned by Handler(). Middlewares run outermost-first in
// installation order; they see every request before the mux does, so an extension can e.g. route by Host
// (<handle>.<domain> -> tunnel ingress) and fall through to next for everything else.
func (s *Server) Use(mw func(next http.Handler) http.Handler) {
	s.rtMu.Lock()
	s.wraps = append(s.wraps, mw)
	s.rtMu.Unlock()
}

// AdminOK reports whether the request carries the configured admin token (header X-Admin-Token or ?token=).
// Always false when no token is configured: admin-only routes are then effectively disabled.
func (s *Server) AdminOK(r *http.Request) bool {
	if s.adminToken == "" {
		return false
	}
	got := r.Header.Get("X-Admin-Token")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) == 1
}

// RequireAdmin wraps h so that requests failing AdminOK get 403 {"error": "admin token required"}.
func (s *Server) RequireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.AdminOK(r) {
			WriteError(w, http.StatusForbidden, "admin token required")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// VerifyRequest checks the A2A request signature headers (X-A2A-Pub / X-A2A-Timestamp / X-A2A-Signature, spec §6)
// against method and path and returns the caller's fingerprint. method/path are the values the caller signed,
// normally r.Method and the route path without query string.
func VerifyRequest(r *http.Request, method, path string) (fp string, err error) {
	return a2a.VerifyReq(
		r.Header.Get(a2a.HeaderPub), method, path,
		r.Header.Get(a2a.HeaderTimestamp), r.Header.Get(a2a.HeaderSignature))
}
