// Vault of the post office: encrypted, content-addressed backup storage per mailbox, so a
// device can restore the identity's latest state even when no other device is online.
//
//	PUT    /vault/{box}/blob/{id}     raw ciphertext body (<= 4 MiB); idempotent; counts against the quota
//	POST   /vault/{box}/has           {"ids":[...]} (<= 10000) -> {"missing":[...]}
//	GET    /vault/{box}/blob/{id}     the blob bytes
//	GET    /vault/{box}/head/{lane}   {lane, version, root, refs, device, updated}
//	PUT    /vault/{box}/head/{lane}   {prev_version, root, refs} -- compare-and-swap on prev_version
//	DELETE /vault/{box}/head/{lane}?prev_version=N   drop a lane (CAS as well)
//	GET    /vault/{box}/heads         {"heads":[...]} every lane
//	GET    /vault/{box}/usage         {bytes, blobs, quota}
//	DELETE /vault/{box}               purge: drop every blob and lane of the mailbox (active device only)
//
// Every route is signed by the mailbox owner exactly like the inbox (VerifyRequest over
// method + URL path; the signed fingerprint must equal {box}). Blob ids are keyed hashes the
// client computes and contents are ciphertext -- the relay interprets neither. The one
// plaintext it parses is the refs blob of a head version (a list of opaque ids, format in
// a2a/vault.go), which is what makes garbage collection possible without reading content.
//
// Lanes:
//
//	main           only the ACTIVE device may advance it (device sessions, active.go:
//	               a non-active device gets 409 kicked, exactly like the mailbox)
//	dev-<device>   only the device whose X-Soulnet-Device equals <device> may advance it
//	               (403 otherwise); a kicked device parks its unsent changes there. The
//	               active device may DELETE any dev lane once it merged it.
//
// Purge (DELETE /vault/{box}) wipes the mailbox's whole vault; it is gated on the active
// device like lane main and is idempotent (a mailbox without a vault answers 200 as well).
// A host uses it when backups stop being needed (e.g. back down to a single device).
//
// A head update is refused (422) unless root, refs and every id refs names are stored, so
// a lane never points at a half-uploaded version.
//
// Garbage collection keeps, per mailbox, every blob that the last N versions (default
// a2a.VaultKeepVersions, see SetVaultKeepVersions)
// versions of any lane reference (their root, their refs blob and every id the refs list
// names) plus every blob written -- or confirmed through has / an idempotent put -- within
// the last 24 hours (so an upload in flight, or a "has says present, skip it" decision, is
// never undercut). It runs asynchronously after head updates (debounced, at most once per
// vaultGCEvery per mailbox) and synchronously once when an upload would exceed the quota.
// It aborts, deleting nothing, if any retained refs blob is unreadable.
//
// Storage: blob bytes go to a VaultBlobStore (vaultstore.go; default: dataDir/vault/<box>/
// blobs/<id[:2]>/<id>, uploads staged in tmp/). Metadata always stays on the relay's disk:
// dataDir/vault/<box>/heads/<lane>.json (atomic replace) and index.jsonl, the blob index
// (vaultindex.go: id -> size and write time). has, the completeness check of a head update,
// quota and collection consult the index only; the store is read for GET blob and for refs
// blobs, and written by uploads, collection and purge. A mailbox is loaded (heads + index,
// usage summed from the index) the first time it is touched after start.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

const (
	// DefaultVaultQuota is the per-mailbox vault quota unless SetVaultQuota says otherwise.
	DefaultVaultQuota int64 = 10 << 30
	// vaultGrace protects recently written / confirmed blobs from garbage collection.
	vaultGrace = 24 * time.Hour
	// vaultGCDelay debounces the collection that follows a head update.
	vaultGCDelay = 30 * time.Second
	// vaultGCEvery rate-limits collections per mailbox.
	vaultGCEvery = 10 * time.Minute
	// maxVaultLanes bounds the lanes of one mailbox (each retains up to three versions).
	maxVaultLanes = 64
	// maxVaultMissingReported caps the ids listed in a 422 missing-blobs verdict.
	maxVaultMissingReported = 100
)

// vaultRev is one version of a lane.
type vaultRev struct {
	Version int64     `json:"version"`
	Root    string    `json:"root"`
	Refs    string    `json:"refs"`
	Device  string    `json:"device,omitempty"`
	Updated time.Time `json:"updated"`
}

// vaultHeadRec is heads/<lane>.json: the current version plus the older retained ones.
type vaultHeadRec struct {
	vaultRev
	History []vaultRev `json:"history,omitempty"` // previous versions, newest first, at most keep-1 (SetVaultKeepVersions)
}

func (h *vaultHeadRec) public(lane string) a2a.VaultHead {
	return a2a.VaultHead{Lane: lane, Version: h.Version, Root: h.Root, Refs: h.Refs, Device: h.Device, Updated: h.Updated}
}

// vaultBox is the in-memory side of one mailbox's vault. mu serialises every mutation of
// the box (index updates, head updates, collection, purge) and every head / index read.
// Uploads store their bytes outside mu, holding a quota reservation meanwhile.
type vaultBox struct {
	mu          sync.Mutex
	loaded      bool
	index       map[string]vaultBlobMeta // stored blobs (vaultindex.go)
	indexLines  int                      // lines in index.jsonl (compaction trigger)
	bytes       int64                    // sum of index sizes
	blobs       int64                    // len(index)
	reserved    int64                    // bytes of uploads in flight (counted against the quota)
	inflight    map[string]int           // ids of uploads in flight -> count
	gen         uint64                   // bumped by purge: an upload that started before it is discarded
	heads       map[string]*vaultHeadRec
	lastGC      time.Time
	gcScheduled bool
}

// SetVaultQuota sets the per-mailbox vault quota in bytes (<= 0 restores DefaultVaultQuota).
func (s *Server) SetVaultQuota(n int64) {
	if n <= 0 {
		n = DefaultVaultQuota
	}
	s.vMu.Lock()
	s.vaultQuota = n
	s.vMu.Unlock()
}

// SetVaultKeepVersions sets how many versions per lane (the current one included) keep
// their blobs alive through garbage collection (<= 0 restores a2a.VaultKeepVersions).
// Lowering it takes effect for existing lanes at their next collection: older retained
// versions stop protecting their blobs, and are trimmed from the head record at the next
// update of the lane.
func (s *Server) SetVaultKeepVersions(n int) {
	if n <= 0 {
		n = a2a.VaultKeepVersions
	}
	s.vMu.Lock()
	s.vaultKeep = n
	s.vMu.Unlock()
}

// VaultKeepVersions returns the number of versions per lane that garbage collection retains.
func (s *Server) VaultKeepVersions() int {
	s.vMu.Lock()
	defer s.vMu.Unlock()
	return s.vaultKeep
}

// VaultQuota returns the per-mailbox vault quota in bytes.
func (s *Server) VaultQuota() int64 {
	s.vMu.Lock()
	defer s.vMu.Unlock()
	return s.vaultQuota
}

// ParseByteSize parses a size such as "10737418240", "10G", "10GB", "10GiB" or "500M".
// Units are binary (K = 1024); case-insensitive; no fractions.
func ParseByteSize(v string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(v))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q (want e.g. 10G, 500M or a byte count)", v)
	}
	if n > (1<<62)/mult {
		return 0, fmt.Errorf("size %q is too large", v)
	}
	return n * mult, nil
}

// SetVaultBlobStore makes store hold the vault's blob bytes (nil restores the default disk
// store). Call it before serving. Blobs the relay already kept on local disk are not
// visible through another store until MigrateVaultBlobs moved them; until then the
// mailboxes concerned answer 500 rather than report blobs the store does not hold.
func (s *Server) SetVaultBlobStore(store VaultBlobStore) {
	if store == nil {
		store = s.vaultDisk
	}
	s.vMu.Lock()
	s.vaultStore = store
	s.vMu.Unlock()
}

func (s *Server) vaultBlobs() VaultBlobStore {
	s.vMu.Lock()
	defer s.vMu.Unlock()
	return s.vaultStore
}

// vaultStoreIsDisk reports whether blobs live in this relay's own on-disk layout.
func (s *Server) vaultStoreIsDisk() bool {
	d, ok := s.vaultBlobs().(*DiskVaultBlobStore)
	return ok && d.root == s.vaultDisk.root
}

func (s *Server) vaultBoxDir(box string) string { return filepath.Join(s.dataDir, "vault", box) }

func (s *Server) vaultHeadPath(box, lane string) string {
	return filepath.Join(s.vaultBoxDir(box), "heads", lane+".json")
}

// vaultBoxFor returns the in-memory record of box (created empty on first use; no disk access).
func (s *Server) vaultBoxFor(box string) *vaultBox {
	s.vMu.Lock()
	defer s.vMu.Unlock()
	vb := s.vboxes[box]
	if vb == nil {
		vb = &vaultBox{}
		s.vboxes[box] = vb
	}
	return vb
}

// vaultLoadLocked reads box's heads and blob index the first time (the index is rebuilt
// from the on-disk blob layout when missing, see vaultLoadIndexLocked). A mailbox without
// a vault directory loads as empty and creates no files. Caller holds vb.mu.
func (s *Server) vaultLoadLocked(box string, vb *vaultBox) error {
	return s.vaultLoadModeLocked(box, vb, false)
}

func (s *Server) vaultLoadModeLocked(box string, vb *vaultBox, migrate bool) error {
	if vb.loaded {
		return nil
	}
	if err := s.vaultLoadIndexLocked(box, vb, migrate); err != nil {
		vb.index = nil
		return err
	}
	heads := map[string]*vaultHeadRec{}
	entries, err := os.ReadDir(filepath.Join(s.vaultBoxDir(box), "heads"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("vault: read heads: %w", err)
	}
	for _, e := range entries {
		lane, ok := strings.CutSuffix(e.Name(), ".json")
		if e.IsDir() || !ok || !a2a.ValidVaultLane(lane) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.vaultBoxDir(box), "heads", e.Name()))
		if err != nil {
			return fmt.Errorf("vault: read head %s: %w", lane, err)
		}
		var h vaultHeadRec
		if err := json.Unmarshal(raw, &h); err != nil || h.Version <= 0 {
			// Refuse to go on rather than drop the lane: a lane we cannot read would let the
			// collector delete the blobs it protects.
			return fmt.Errorf("vault: head %s is unreadable", lane)
		}
		heads[lane] = &h
	}
	vb.recount()
	vb.heads, vb.loaded = heads, true
	return nil
}

// mountVault registers the vault routes; called from mountCore.
func (s *Server) mountVault(must func(error)) {
	must(s.HandleFunc("PUT /vault/{box}/blob/{id}", s.vaultPutBlob))
	must(s.HandleFunc("GET /vault/{box}/blob/{id}", s.vaultGetBlob))
	must(s.HandleFunc("POST /vault/{box}/has", s.vaultHas))
	must(s.HandleFunc("GET /vault/{box}/head/{lane}", s.vaultGetHead))
	must(s.HandleFunc("PUT /vault/{box}/head/{lane}", s.vaultPutHead))
	must(s.HandleFunc("DELETE /vault/{box}/head/{lane}", s.vaultDeleteHead))
	must(s.HandleFunc("GET /vault/{box}/heads", s.vaultListHeads))
	must(s.HandleFunc("GET /vault/{box}/usage", s.vaultUsage))
	must(s.HandleFunc("DELETE /vault/{box}", s.vaultPurge))
}

// vaultAuth validates {box} and the owner signature over method + URL path. It answers
// the error itself and returns ok=false on failure.
func (s *Server) vaultAuth(w http.ResponseWriter, r *http.Request) (box string, ok bool) {
	box = r.PathValue("box")
	if !SafeBox(box) {
		WriteError(w, 400, "invalid box")
		return "", false
	}
	if err := s.authBox(r, r.Method, r.URL.Path, box); err != nil {
		WriteError(w, 401, err.Error())
		return "", false
	}
	return box, true
}

// vaultOpen returns box's record, loaded; on failure it answers 500 and returns nil.
// On success the caller holds vb.mu and must unlock it.
func (s *Server) vaultOpen(w http.ResponseWriter, box string) *vaultBox {
	vb := s.vaultBoxFor(box)
	vb.mu.Lock()
	if err := s.vaultLoadLocked(box, vb); err != nil {
		vb.mu.Unlock()
		log.Printf("[relay] vault box=%s load failed: %v", a2a.ShortFp(box), err)
		WriteError(w, 500, err.Error())
		return nil
	}
	return vb
}

// vaultTouchLocked resets the grace clock of the given stored blobs (index + log). A failed log
// append only costs the persisted clock (the in-memory one is set), so it is logged, not
// fatal. Caller holds vb.mu.
func (s *Server) vaultTouchLocked(box string, vb *vaultBox, ids []string) {
	if len(ids) == 0 {
		return
	}
	now := time.Now().Unix()
	for _, id := range ids {
		m := vb.index[id]
		m.At = now
		vb.index[id] = m
	}
	if err := s.vaultIndexAppendLocked(box, vb, vaultIndexRec{Touch: ids, At: now}); err != nil {
		log.Printf("[relay] vault box=%s index touch failed: %v", a2a.ShortFp(box), err)
	}
}

// vaultPutBlob: PUT /vault/{box}/blob/{id}, body = raw blob bytes.
//
// The body is read into memory (<= 4 MiB) and handed to the store outside the box lock;
// meanwhile its size is reserved against the quota and the id is marked in flight (so a
// collection never deletes it under the upload). The index learns the blob only after
// the store has it.
func (s *Server) vaultPutBlob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a2a.ValidVaultID(id) {
		WriteError(w, 400, "invalid blob id (want 64 lowercase hex characters)")
		return
	}
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	if r.ContentLength > a2a.MaxVaultBlobBytes {
		WriteError(w, http.StatusRequestEntityTooLarge, "blob exceeds 4 MiB")
		return
	}
	quota := s.VaultQuota()
	existed := func(vb *vaultBox) {
		s.vaultTouchLocked(box, vb, []string{id})
		vb.mu.Unlock()
		WriteJSON(w, 200, map[string]any{"ok": true, "existed": true})
	}

	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	if _, ok := vb.index[id]; ok { // idempotent: already stored
		s.vaultTouchLocked(box, vb, []string{id})
		vb.mu.Unlock()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, a2a.MaxVaultBlobBytes+1))
		WriteJSON(w, 200, map[string]any{"ok": true, "existed": true})
		return
	}
	if r.ContentLength > 0 && vb.bytes+vb.reserved+r.ContentLength > quota && !s.vaultReclaimLocked(box, vb, r.ContentLength, quota) {
		vb.mu.Unlock()
		WriteError(w, http.StatusRequestEntityTooLarge, a2a.VaultQuotaCode)
		return
	}
	vb.mu.Unlock()

	// Read the body outside the lock (a 4 MiB body on a slow link takes a while).
	data, err := io.ReadAll(io.LimitReader(r.Body, a2a.MaxVaultBlobBytes+1))
	if err != nil {
		WriteError(w, 400, "failed to read blob body")
		return
	}
	n := int64(len(data))
	if n > a2a.MaxVaultBlobBytes {
		WriteError(w, http.StatusRequestEntityTooLarge, "blob exceeds 4 MiB")
		return
	}
	if n == 0 {
		WriteError(w, 400, "empty blob")
		return
	}

	vb.mu.Lock()
	if !vb.loaded { // a failed purge left the box to be rescanned
		if err := s.vaultLoadLocked(box, vb); err != nil {
			vb.mu.Unlock()
			WriteError(w, 500, err.Error())
			return
		}
	}
	if _, ok := vb.index[id]; ok { // a concurrent upload of the same id won
		existed(vb)
		return
	}
	if vb.bytes+vb.reserved+n > quota && !s.vaultReclaimLocked(box, vb, n, quota) {
		vb.mu.Unlock()
		WriteError(w, http.StatusRequestEntityTooLarge, a2a.VaultQuotaCode)
		return
	}
	gen := vb.gen
	vb.reserved += n
	if vb.inflight == nil {
		vb.inflight = map[string]int{}
	}
	vb.inflight[id]++
	store := s.vaultBlobs()
	vb.mu.Unlock()

	putErr := store.Put(r.Context(), box, id, data)

	vb.mu.Lock()
	defer vb.mu.Unlock()
	vb.reserved -= n
	if vb.inflight[id]--; vb.inflight[id] <= 0 {
		delete(vb.inflight, id)
	}
	if putErr != nil {
		log.Printf("[relay] vault box=%s blob %s store put failed: %v", a2a.ShortFp(box), id[:8], putErr)
		WriteError(w, 500, "failed to store blob")
		return
	}
	if vb.gen != gen || !vb.loaded {
		// The vault was purged while we uploaded: do not resurrect the blob. Drop our bytes
		// unless the new vault already knows the id or another upload of it is in flight.
		if _, ok := vb.index[id]; !ok && vb.inflight[id] == 0 {
			if err := store.Delete(context.Background(), box, id); err != nil {
				log.Printf("[relay] vault box=%s blob %s cleanup after purge failed: %v", a2a.ShortFp(box), id[:8], err)
			}
		}
		WriteError(w, 500, "the vault was purged during the upload")
		return
	}
	if _, ok := vb.index[id]; ok { // a concurrent upload of the same id finished first
		s.vaultTouchLocked(box, vb, []string{id})
		WriteJSON(w, 200, map[string]any{"ok": true, "existed": true})
		return
	}
	at := time.Now().Unix()
	vb.index[id] = vaultBlobMeta{Size: n, At: at}
	if err := s.vaultIndexAppendLocked(box, vb, vaultIndexRec{Put: id, Size: n, At: at}); err != nil {
		// Not indexed = not stored as far as the vault is concerned (the store copy is an
		// invisible leftover that a retry overwrites).
		delete(vb.index, id)
		log.Printf("[relay] vault box=%s blob %s index append failed: %v", a2a.ShortFp(box), id[:8], err)
		WriteError(w, 500, "failed to record blob")
		return
	}
	vb.bytes += n
	vb.blobs++
	WriteJSON(w, 200, map[string]any{"ok": true, "existed": false, "bytes": n})
}

// vaultReclaimLocked runs one synchronous collection when an upload of need bytes would
// exceed quota (at most once per vaultGCEvery) and reports whether it now fits. Caller holds vb.mu.
func (s *Server) vaultReclaimLocked(box string, vb *vaultBox, need, quota int64) bool {
	if time.Since(vb.lastGC) < s.vaultGCEvery {
		return false
	}
	if _, err := s.vaultGCLocked(box, vb); err != nil {
		log.Printf("[relay] vault box=%s gc on quota pressure failed: %v", a2a.ShortFp(box), err)
	}
	return vb.bytes+vb.reserved+need <= quota
}

// vaultGetBlob: GET /vault/{box}/blob/{id} -> the blob bytes (404 when not stored).
func (s *Server) vaultGetBlob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !a2a.ValidVaultID(id) {
		WriteError(w, 400, "invalid blob id (want 64 lowercase hex characters)")
		return
	}
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	_, indexed := vb.index[id]
	vb.mu.Unlock()
	if !indexed {
		WriteError(w, 404, a2a.VaultNoBlobCode)
		return
	}
	data, err := s.vaultBlobs().Get(r.Context(), box, id)
	if err != nil {
		if errors.Is(err, ErrVaultBlobNotFound) {
			log.Printf("[relay] vault box=%s blob %s is indexed but the store has no bytes", a2a.ShortFp(box), id[:8])
			WriteError(w, 404, a2a.VaultNoBlobCode)
			return
		}
		log.Printf("[relay] vault box=%s blob %s store get failed: %v", a2a.ShortFp(box), id[:8], err)
		WriteError(w, 500, "failed to read blob")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// vaultHas: POST /vault/{box}/has {"ids":[...]} -> {"missing":[...]} in request order.
// Answered from the index alone. Present blobs get their grace clock refreshed, so a
// client that skips uploading them cannot lose them to a collection before it publishes
// the head that references them.
func (s *Server) vaultHas(w http.ResponseWriter, r *http.Request) {
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {ids}")
		return
	}
	if len(body.IDs) > a2a.MaxVaultHasIDs {
		WriteError(w, 400, fmt.Sprintf("at most %d ids per query", a2a.MaxVaultHasIDs))
		return
	}
	for _, id := range body.IDs {
		if !a2a.ValidVaultID(id) {
			WriteError(w, 400, "invalid blob id (want 64 lowercase hex characters)")
			return
		}
	}
	vb := s.vaultOpen(w, box) // holding the lock excludes a collection between "present" and the touch
	if vb == nil {
		return
	}
	missing := []string{}
	var present []string
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if _, ok := vb.index[id]; ok {
			if !seen[id] {
				present = append(present, id)
				seen[id] = true
			}
		} else {
			missing = append(missing, id)
		}
	}
	s.vaultTouchLocked(box, vb, present)
	vb.mu.Unlock()
	WriteJSON(w, 200, map[string]any{"missing": missing})
}

// vaultLane validates {lane}; it answers 400 itself and returns ok=false when invalid.
func vaultLane(w http.ResponseWriter, r *http.Request) (string, bool) {
	lane := r.PathValue("lane")
	if !a2a.ValidVaultLane(lane) {
		WriteError(w, 400, `invalid lane (want "main" or "dev-<device id>")`)
		return "", false
	}
	return lane, true
}

// vaultLaneGate applies the lane write rule. It answers the refusal itself and returns
// false when the caller may not write lane. allowActiveOnDev lets the active device also
// write (delete) other devices' dev lanes.
func (s *Server) vaultLaneGate(w http.ResponseWriter, r *http.Request, box, lane string, allowActiveOnDev bool) bool {
	if lane == a2a.VaultMainLane {
		if ad := s.deviceGate(r, box); ad != nil {
			writeKicked(w, ad)
			return false
		}
		return true
	}
	device := strings.TrimSpace(r.Header.Get(a2a.HeaderDevice))
	if device != "" && device == strings.TrimPrefix(lane, a2a.VaultDevLanePrefix) {
		return true
	}
	if allowActiveOnDev {
		if ad := s.deviceGate(r, box); ad != nil {
			writeKicked(w, ad)
			return false
		}
		return true
	}
	WriteError(w, 403, "lane belongs to another device")
	return false
}

// vaultGetHead: GET /vault/{box}/head/{lane} -> the current version (404 no such lane).
func (s *Server) vaultGetHead(w http.ResponseWriter, r *http.Request) {
	lane, ok := vaultLane(w, r)
	if !ok {
		return
	}
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	h := vb.heads[lane]
	var out a2a.VaultHead
	if h != nil {
		out = h.public(lane)
	}
	vb.mu.Unlock()
	if h == nil {
		WriteError(w, 404, a2a.VaultNoLaneCode)
		return
	}
	WriteJSON(w, 200, out)
}

// vaultListHeads: GET /vault/{box}/heads -> {"heads":[...]} sorted by lane.
func (s *Server) vaultListHeads(w http.ResponseWriter, r *http.Request) {
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	out := make([]a2a.VaultHead, 0, len(vb.heads))
	for lane, h := range vb.heads {
		out = append(out, h.public(lane))
	}
	vb.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Lane < out[j].Lane })
	WriteJSON(w, 200, map[string]any{"heads": out})
}

// vaultUsage: GET /vault/{box}/usage -> {bytes, blobs, quota}.
func (s *Server) vaultUsage(w http.ResponseWriter, r *http.Request) {
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	out := a2a.VaultUsage{Bytes: vb.bytes, Blobs: vb.blobs, Quota: s.VaultQuota()}
	vb.mu.Unlock()
	WriteJSON(w, 200, out)
}

func writeVaultConflict(w http.ResponseWriter, h *vaultHeadRec) {
	var v int64
	root := ""
	if h != nil {
		v, root = h.Version, h.Root
	}
	WriteJSON(w, http.StatusConflict, map[string]any{"error": a2a.VaultConflictCode, "version": v, "root": root})
}

// vaultPutHead: PUT /vault/{box}/head/{lane} {prev_version, root, refs}. prev_version must
// equal the lane's current version (0 = the lane must not exist yet); the new version is
// prev_version+1.
func (s *Server) vaultPutHead(w http.ResponseWriter, r *http.Request) {
	lane, ok := vaultLane(w, r)
	if !ok {
		return
	}
	var body struct {
		PrevVersion int64  `json:"prev_version"`
		Root        string `json:"root"`
		Refs        string `json:"refs"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		WriteError(w, 400, "request body must be {prev_version, root, refs}")
		return
	}
	if body.PrevVersion < 0 || !a2a.ValidVaultID(body.Root) || !a2a.ValidVaultID(body.Refs) {
		WriteError(w, 400, "prev_version must be >= 0 and root / refs must be blob ids")
		return
	}
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	if !s.vaultLaneGate(w, r, box, lane, false) {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	cur := vb.heads[lane]
	var curV int64
	if cur != nil {
		curV = cur.Version
	}
	if body.PrevVersion != curV {
		vb.mu.Unlock()
		writeVaultConflict(w, cur) // records are replaced, never mutated: safe to read unlocked
		return
	}
	if cur == nil && len(vb.heads) >= maxVaultLanes {
		vb.mu.Unlock()
		WriteError(w, 400, fmt.Sprintf("too many lanes (at most %d per mailbox)", maxVaultLanes))
		return
	}
	// The version must be complete: root, refs and everything refs names are stored
	// (per the index; only the refs blob itself is read from the store).
	var missing []string
	for _, id := range []string{body.Root, body.Refs} {
		if _, ok := vb.index[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		raw, err := s.vaultBlobs().Get(r.Context(), box, body.Refs)
		if err != nil {
			vb.mu.Unlock()
			log.Printf("[relay] vault box=%s refs blob %s unreadable: %v", a2a.ShortFp(box), body.Refs[:8], err)
			WriteError(w, 500, "failed to read the refs blob")
			return
		}
		ids, err := a2a.ParseVaultRefs(raw)
		if err != nil {
			vb.mu.Unlock()
			WriteError(w, http.StatusUnprocessableEntity, a2a.VaultBadRefsCode)
			return
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if _, ok := vb.index[id]; !ok && !seen[id] {
				missing = append(missing, id)
			}
			seen[id] = true
		}
	}
	if len(missing) > 0 {
		vb.mu.Unlock()
		shown := missing
		if len(shown) > maxVaultMissingReported {
			shown = shown[:maxVaultMissingReported]
		}
		WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": a2a.VaultMissingCode, "missing": shown, "count": len(missing)})
		return
	}
	next := &vaultHeadRec{vaultRev: vaultRev{
		Version: curV + 1,
		Root:    body.Root,
		Refs:    body.Refs,
		Device:  strings.TrimSpace(r.Header.Get(a2a.HeaderDevice)),
		Updated: time.Now().UTC(),
	}}
	if cur != nil {
		next.History = append([]vaultRev{cur.vaultRev}, cur.History...)
		if keep := s.VaultKeepVersions(); len(next.History) > keep-1 {
			next.History = next.History[:keep-1]
		}
	}
	raw, _ := json.Marshal(next)
	if err := writeFileAtomic(s.vaultHeadPath(box, lane), raw); err != nil {
		vb.mu.Unlock()
		WriteError(w, 500, err.Error())
		return
	}
	vb.heads[lane] = next
	out := next.public(lane)
	vb.mu.Unlock()
	s.vaultScheduleGC(box)
	WriteJSON(w, 200, map[string]any{"ok": true, "head": out})
}

// vaultDeleteHead: DELETE /vault/{box}/head/{lane}?prev_version=N drops a lane whose
// current version is N. main: active device only. dev-<id>: that device, or the active
// device (which drops a dev lane after merging it).
func (s *Server) vaultDeleteHead(w http.ResponseWriter, r *http.Request) {
	lane, ok := vaultLane(w, r)
	if !ok {
		return
	}
	prev, err := strconv.ParseInt(r.URL.Query().Get("prev_version"), 10, 64)
	if err != nil || prev <= 0 {
		WriteError(w, 400, "prev_version (the lane's current version) is required")
		return
	}
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	if !s.vaultLaneGate(w, r, box, lane, true) {
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	cur := vb.heads[lane]
	if cur == nil {
		vb.mu.Unlock()
		WriteError(w, 404, a2a.VaultNoLaneCode)
		return
	}
	if cur.Version != prev {
		vb.mu.Unlock()
		writeVaultConflict(w, cur)
		return
	}
	if err := os.Remove(s.vaultHeadPath(box, lane)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		vb.mu.Unlock()
		WriteError(w, 500, err.Error())
		return
	}
	delete(vb.heads, lane)
	vb.mu.Unlock()
	s.vaultScheduleGC(box)
	WriteJSON(w, 200, map[string]any{"ok": true})
}

// vaultPurge: DELETE /vault/{box} drops every blob and lane of box and resets its usage.
// Active device only (409 kicked otherwise, legacy callers pass while nobody claimed the
// mailbox). Idempotent: answers 200 with what was freed (zero when there was no vault).
// An upload racing the purge fails rather than resurrecting data.
//
// Order: the index is emptied and the lanes dropped first, then the store deletes the
// bytes. If the store fails, the vault is already empty to clients and the stray bytes
// are invisible; a retried purge deletes them.
func (s *Server) vaultPurge(w http.ResponseWriter, r *http.Request) {
	box, ok := s.vaultAuth(w, r)
	if !ok {
		return
	}
	if ad := s.deviceGate(r, box); ad != nil {
		writeKicked(w, ad)
		return
	}
	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	defer vb.mu.Unlock()
	freedBytes, freedBlobs := vb.bytes, vb.blobs
	vb.gen++
	fail := func(what string, err error) {
		vb.loaded = false // partially removed: reload on next use instead of trusting memory
		log.Printf("[relay] vault box=%s purge failed (%s): %v", a2a.ShortFp(box), what, err)
		WriteError(w, 500, "purge failed: "+what)
	}
	dir := s.vaultBoxDir(box)
	if _, err := os.Stat(dir); err == nil {
		vb.index = map[string]vaultBlobMeta{}
		if err := s.vaultIndexCompactLocked(box, vb); err != nil {
			fail("index", err)
			return
		}
		if err := os.RemoveAll(filepath.Join(dir, "heads")); err != nil {
			fail("heads", err)
			return
		}
	}
	vb.index, vb.heads = map[string]vaultBlobMeta{}, map[string]*vaultHeadRec{}
	vb.recount()
	if err := s.vaultBlobs().DeleteBox(r.Context(), box); err != nil {
		// Metadata is already empty (and stays loaded): clients see an empty vault.
		log.Printf("[relay] vault box=%s purge: store delete failed, %d blob(s) left behind until a retry: %v", a2a.ShortFp(box), freedBlobs, err)
		WriteError(w, 500, "purge failed: store")
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		fail("metadata", err)
		return
	}
	vb.indexLines = 0
	log.Printf("[relay] vault box=%s purged (%d blob(s), %d bytes)", a2a.ShortFp(box), freedBlobs, freedBytes)
	WriteJSON(w, 200, map[string]any{"ok": true, "bytes": freedBytes, "blobs": freedBlobs})
}

// writeFileAtomic replaces path with data (temp file in the same directory, fsync, rename).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// vaultScheduleGC arranges one asynchronous collection of box: vaultGCDelay from now, but
// not sooner than vaultGCEvery after the previous one. A negative vaultGCDelay disables it.
func (s *Server) vaultScheduleGC(box string) {
	if s.vaultGCDelay < 0 {
		return
	}
	vb := s.vaultBoxFor(box)
	vb.mu.Lock()
	if vb.gcScheduled {
		vb.mu.Unlock()
		return
	}
	delay := s.vaultGCDelay
	if next := time.Until(vb.lastGC.Add(s.vaultGCEvery)); next > delay {
		delay = next
	}
	vb.gcScheduled = true
	vb.mu.Unlock()
	time.AfterFunc(delay, func() {
		vb.mu.Lock()
		vb.gcScheduled = false
		n, err := s.vaultGCLocked(box, vb)
		vb.mu.Unlock()
		if err != nil {
			log.Printf("[relay] vault box=%s gc failed: %v", a2a.ShortFp(box), err)
		} else if n > 0 {
			log.Printf("[relay] vault box=%s gc removed %d blob(s)", a2a.ShortFp(box), n)
		}
	})
}

// vaultGC collects box synchronously (tests, operators) and returns the blobs removed.
func (s *Server) vaultGC(box string) (int, error) {
	vb := s.vaultBoxFor(box)
	vb.mu.Lock()
	defer vb.mu.Unlock()
	return s.vaultGCLocked(box, vb)
}

// vaultGCLocked deletes every indexed blob of box that no retained lane version
// references, that was not written / confirmed within vaultGrace and that no upload has
// in flight, plus stale upload temp files of the disk layout. The candidates come from
// the index (the store is never listed); the refs blobs of retained versions are read
// from the store. Their del line is logged before the store deletes them. Caller holds vb.mu.
func (s *Server) vaultGCLocked(box string, vb *vaultBox) (int, error) {
	vb.lastGC = time.Now()
	if err := s.vaultLoadLocked(box, vb); err != nil {
		return 0, err
	}
	store := s.vaultBlobs()
	ctx := context.Background()
	live := map[string]bool{}
	keep := s.VaultKeepVersions()
	for lane, h := range vb.heads {
		hist := h.History
		if len(hist) > keep-1 {
			hist = hist[:keep-1]
		}
		for _, rev := range append([]vaultRev{h.vaultRev}, hist...) {
			live[rev.Root], live[rev.Refs] = true, true
			raw, err := store.Get(ctx, box, rev.Refs)
			if err != nil {
				return 0, fmt.Errorf("gc aborted: refs of lane %s version %d unreadable: %w", lane, rev.Version, err)
			}
			ids, err := a2a.ParseVaultRefs(raw)
			if err != nil {
				return 0, fmt.Errorf("gc aborted: refs of lane %s version %d: %w", lane, rev.Version, err)
			}
			for _, id := range ids {
				live[id] = true
			}
		}
	}
	cutoff := time.Now().Add(-s.vaultGrace).Unix()
	var victims []string
	for id, m := range vb.index {
		if !live[id] && m.At < cutoff && vb.inflight[id] == 0 {
			victims = append(victims, id)
		}
	}
	sort.Strings(victims)
	if len(victims) > 0 {
		for _, id := range victims {
			delete(vb.index, id)
		}
		if err := s.vaultIndexAppendLocked(box, vb, vaultIndexRec{Del: victims}); err != nil {
			// Put them back: nothing is deleted from the store unless the index forgot it first.
			if rerr := s.vaultLoadIndexLocked(box, vb, false); rerr != nil {
				vb.loaded = false
			}
			vb.recount()
			return 0, fmt.Errorf("gc aborted: %w", err)
		}
		vb.recount()
		for _, id := range victims {
			if err := store.Delete(ctx, box, id); err != nil {
				// Forgotten by the index already: the bytes are an invisible leftover.
				log.Printf("[relay] vault box=%s gc: store delete of %s failed: %v", a2a.ShortFp(box), id[:8], err)
			}
		}
		if err := s.vaultIndexCompactLocked(box, vb); err != nil {
			log.Printf("[relay] vault box=%s gc: index compaction failed: %v", a2a.ShortFp(box), err)
		}
	}
	tmpDir := filepath.Join(s.vaultBoxDir(box), "tmp")
	tmps, _ := os.ReadDir(tmpDir)
	for _, f := range tmps {
		if info, err := f.Info(); err == nil && !f.IsDir() && info.ModTime().Unix() < cutoff {
			_ = os.Remove(filepath.Join(tmpDir, f.Name()))
		}
	}
	return len(victims), nil
}

// VaultMigration is what MigrateVaultBlobs moved.
type VaultMigration struct {
	Boxes int   // mailboxes that had blobs on local disk
	Blobs int   // blobs moved
	Bytes int64 // bytes moved
}

// MigrateVaultBlobs moves every blob still kept in the on-disk layout
// (dataDir/vault/<box>/blobs) into the configured VaultBlobStore and deletes the local
// copies; the index keeps each blob's size and write time (it is rebuilt from the files
// first when missing). It is a no-op when the configured store is the disk layout itself.
// Call it after SetVaultBlobStore and before serving; it is idempotent and resumable (a
// run cut short leaves the rest on disk, and those mailboxes answer 500 until a later run
// finishes them). logf, when set, receives one line per mailbox moved.
func (s *Server) MigrateVaultBlobs(ctx context.Context, logf func(format string, args ...any)) (VaultMigration, error) {
	var out VaultMigration
	if s.vaultStoreIsDisk() {
		return out, nil
	}
	entries, err := os.ReadDir(filepath.Join(s.dataDir, "vault"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	for _, e := range entries {
		box := e.Name()
		if !e.IsDir() || !SafeBox(box) || !s.vaultDisk.hasBlobsDir(box) {
			continue
		}
		n, b, err := s.vaultMigrateBox(ctx, box)
		out.Blobs += n
		out.Bytes += b
		if n > 0 || err == nil {
			out.Boxes++
		}
		if err != nil {
			return out, fmt.Errorf("vault: migrate box %s: %w", a2a.ShortFp(box), err)
		}
		if logf != nil {
			logf("vault box=%s: moved %d blob(s), %d bytes from local disk to the vault store", a2a.ShortFp(box), n, b)
		}
	}
	return out, nil
}

func (s *Server) vaultMigrateBox(ctx context.Context, box string) (int, int64, error) {
	vb := s.vaultBoxFor(box)
	vb.mu.Lock()
	defer vb.mu.Unlock()
	if err := s.vaultLoadModeLocked(box, vb, true); err != nil {
		return 0, 0, err
	}
	store := s.vaultBlobs()
	moved, bytes := 0, int64(0)
	err := s.vaultDisk.walk(box, func(id string, size int64, mod time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := s.vaultDisk.Get(ctx, box, id)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return s.vaultDisk.Delete(ctx, box, id) // never a valid blob
		}
		if err := store.Put(ctx, box, id, data); err != nil {
			return err
		}
		if _, ok := vb.index[id]; !ok { // stored but never indexed (crash before the put line)
			vb.index[id] = vaultBlobMeta{Size: int64(len(data)), At: mod.Unix()}
			if err := s.vaultIndexAppendLocked(box, vb, vaultIndexRec{Put: id, Size: int64(len(data)), At: mod.Unix()}); err != nil {
				delete(vb.index, id)
				return err
			}
		}
		if err := s.vaultDisk.Delete(ctx, box, id); err != nil {
			return err
		}
		moved++
		bytes += int64(len(data))
		return nil
	})
	vb.recount()
	if err != nil {
		return moved, bytes, err
	}
	if err := s.vaultDisk.DeleteBox(ctx, box); err != nil {
		return moved, bytes, err
	}
	if err := s.vaultIndexCompactLocked(box, vb); err != nil {
		return moved, bytes, err
	}
	return moved, bytes, nil
}
