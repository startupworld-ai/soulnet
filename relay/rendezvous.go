// Pairing rendezvous of the post office: a short-lived, unauthenticated drop box two
// devices use to exchange ciphertext while one of them joins the identity (spec: join =
// one pairing). Nothing here is readable by the relay -- both the rendezvous id and the
// encryption key are derived from a short-lived pairing code the user carries between the
// two devices; the relay only stores opaque blobs for ten minutes.
//
//	POST   /rendezvous/{id}   {seq, data}      put one blob (data base64, <= 4 MB decoded)
//	GET    /rendezvous/{id}?since=<seq>&wait=<s>  blobs with seq > since, long-polls up to 55 s
//	DELETE /rendezvous/{id}                    drop the rendezvous
//
// Blobs live on disk under rendezvous/<id>/<seq>.bin (a join bundle may be tens of MB;
// keeping it in memory would make a small relay easy to exhaust). A rendezvous is dropped
// after rendezvousIdle without any put/get, and every rendezvous is dropped when the relay
// starts (a pairing that spans a relay restart simply starts over). Expiry is checked
// lazily on each rendezvous request -- no background goroutine, deterministic in tests.
package relay

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

const (
	// rendezvousIdle is the inactivity after which a rendezvous and its blobs are dropped.
	rendezvousIdle = 10 * time.Minute
	// maxRendezvousBlob caps one decoded blob.
	maxRendezvousBlob = 4 << 20
	// maxRendezvousTotal caps the decoded bytes of one rendezvous.
	maxRendezvousTotal = 64 << 20
	// maxRendezvousBody bounds the JSON request body (base64 inflates the blob by 4/3, plus framing).
	maxRendezvousBody = maxRendezvousBlob/3*4 + 4096
)

// rendezvousIDRe: rendezvous ids are derived tokens, URL and file-name safe, 8..64 characters.
var rendezvousIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidRendezvousID reports whether id is acceptable as a rendezvous id.
func ValidRendezvousID(id string) bool { return rendezvousIDRe.MatchString(id) }

// rvState is the in-memory side of one rendezvous (the blobs are on disk).
type rvState struct {
	last time.Time     // last put/get (idle clock)
	size int64         // decoded bytes stored
	seqs map[int64]int // seq -> decoded size (duplicate detection without touching the disk)
	wake chan struct{} // closed on put / delete to release long-pollers
}

func (s *Server) rendezvousRoot() string { return filepath.Join(s.dataDir, "rendezvous") }

func (s *Server) rendezvousDir(id string) string { return filepath.Join(s.rendezvousRoot(), id) }

func rendezvousFile(seq int64) string { return fmt.Sprintf("%020d.bin", seq) }

// resetRendezvous drops every rendezvous left on disk (called from New: a pairing does not
// survive a relay restart).
func (s *Server) resetRendezvous() { _ = os.RemoveAll(s.rendezvousRoot()) }

// mountRendezvous registers the rendezvous routes; called from mountCore.
func (s *Server) mountRendezvous(must func(error)) {
	must(s.HandleFunc("POST /rendezvous/{id}", s.rendezvousPut))
	must(s.HandleFunc("GET /rendezvous/{id}", s.rendezvousGet))
	must(s.HandleFunc("DELETE /rendezvous/{id}", s.rendezvousDelete))
}

// sweepRendezvousLocked drops every rendezvous idle for longer than rvIdle. Caller holds rvMu.
func (s *Server) sweepRendezvousLocked(now time.Time) {
	for id, st := range s.rendezvous {
		if now.Sub(st.last) > s.rvIdle {
			s.dropRendezvousLocked(id)
		}
	}
}

// dropRendezvousLocked removes one rendezvous (memory + disk) and releases its pollers. Caller holds rvMu.
func (s *Server) dropRendezvousLocked(id string) {
	if st, ok := s.rendezvous[id]; ok {
		close(st.wake)
		delete(s.rendezvous, id)
	}
	_ = os.RemoveAll(s.rendezvousDir(id))
}

// rendezvousPut: POST /rendezvous/{id} {seq, data}.
func (s *Server) rendezvousPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidRendezvousID(id) {
		WriteError(w, 400, "invalid rendezvous id")
		return
	}
	var body struct {
		Seq  int64  `json:"seq"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRendezvousBody)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {seq, data} (data base64, at most 4 MB decoded)")
		return
	}
	if body.Seq <= 0 {
		WriteError(w, 400, "seq must be a positive integer")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		WriteError(w, 400, "data must be base64")
		return
	}
	if len(raw) > maxRendezvousBlob {
		WriteError(w, http.StatusRequestEntityTooLarge, "blob exceeds 4 MB")
		return
	}
	now := time.Now()
	s.rvMu.Lock()
	defer s.rvMu.Unlock()
	s.sweepRendezvousLocked(now)
	st := s.rendezvous[id]
	if st == nil {
		st = &rvState{seqs: map[int64]int{}, wake: make(chan struct{})}
		s.rendezvous[id] = st
	}
	st.last = now
	if _, dup := st.seqs[body.Seq]; dup {
		WriteError(w, http.StatusConflict, "seq already stored")
		return
	}
	if st.size+int64(len(raw)) > maxRendezvousTotal {
		WriteError(w, http.StatusRequestEntityTooLarge, "rendezvous exceeds 64 MB")
		return
	}
	dir := s.rendezvousDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, rendezvousFile(body.Seq)), raw, 0o644); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	st.seqs[body.Seq] = len(raw)
	st.size += int64(len(raw))
	close(st.wake)
	st.wake = make(chan struct{})
	WriteJSON(w, 200, map[string]any{"ok": true, "seq": body.Seq})
}

// readRendezvous returns the blobs of id with seq > since, in seq order (nil when the
// rendezvous does not exist), plus the wake channel to wait on for more.
func (s *Server) readRendezvous(id string, since int64, now time.Time) ([]a2a.RendezvousItem, <-chan struct{}) {
	s.rvMu.Lock()
	s.sweepRendezvousLocked(now)
	st := s.rendezvous[id]
	if st == nil {
		s.rvMu.Unlock()
		return nil, nil
	}
	st.last = now
	wake := st.wake
	seqs := make([]int64, 0, len(st.seqs))
	for seq := range st.seqs {
		if seq > since {
			seqs = append(seqs, seq)
		}
	}
	s.rvMu.Unlock()
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	items := make([]a2a.RendezvousItem, 0, len(seqs))
	for _, seq := range seqs {
		raw, err := os.ReadFile(filepath.Join(s.rendezvousDir(id), rendezvousFile(seq)))
		if err != nil {
			continue // dropped between the index read and the file read
		}
		items = append(items, a2a.RendezvousItem{Seq: seq, Data: raw})
	}
	return items, wake
}

// rendezvousGet: GET /rendezvous/{id}?since=<seq>&wait=<s> -> {"items": [{seq, data}]}.
// An unknown rendezvous is not an error (the reader may arrive before the writer): the
// call waits for it up to wait seconds and answers an empty list.
func (s *Server) rendezvousGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidRendezvousID(id) {
		WriteError(w, 400, "invalid rendezvous id")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait > 55 {
		wait = 55
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		items, wake := s.readRendezvous(id, since, time.Now())
		if len(items) > 0 || wait <= 0 || time.Now().After(deadline) {
			if items == nil {
				items = []a2a.RendezvousItem{}
			}
			WriteJSON(w, 200, map[string]any{"items": items})
			return
		}
		pause := time.Until(deadline)
		if wake == nil && pause > 250*time.Millisecond {
			// Not created yet: poll for its appearance (cheap; a pairing is a handful of requests).
			// A nil wake channel blocks forever in the select below, so the timer is what releases us.
			pause = 250 * time.Millisecond
		}
		select {
		case <-wake:
		case <-time.After(pause):
		case <-r.Context().Done():
			return
		}
	}
}

// rendezvousDelete: DELETE /rendezvous/{id}. Idempotent.
func (s *Server) rendezvousDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidRendezvousID(id) {
		WriteError(w, 400, "invalid rendezvous id")
		return
	}
	s.rvMu.Lock()
	s.dropRendezvousLocked(id)
	s.rvMu.Unlock()
	WriteJSON(w, 200, map[string]any{"ok": true})
}

// RendezvousCount reports how many rendezvous are alive after sweeping expired ones (diagnostics / tests).
func (s *Server) RendezvousCount() int {
	s.rvMu.Lock()
	defer s.rvMu.Unlock()
	s.sweepRendezvousLocked(time.Now())
	return len(s.rendezvous)
}
