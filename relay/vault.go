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
// Garbage collection keeps, per mailbox, every blob that the last VaultKeepVersions
// versions of any lane reference (their root, their refs blob and every id the refs list
// names) plus every blob written -- or confirmed through has / an idempotent put -- within
// the last 24 hours (so an upload in flight, or a "has says present, skip it" decision, is
// never undercut). It runs asynchronously after head updates (debounced, at most once per
// vaultGCEvery per mailbox) and synchronously once when an upload would exceed the quota.
// It aborts, deleting nothing, if any retained refs blob is unreadable.
//
// Storage: dataDir/vault/<box>/blobs/<id[:2]>/<id>, heads/<lane>.json (atomic replace),
// tmp/ (uploads in progress, renamed into place). Usage is counted by scanning a mailbox's
// blobs the first time it is touched after start, then kept in memory.
package relay

import (
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
	History []vaultRev `json:"history,omitempty"` // previous versions, newest first, at most VaultKeepVersions-1
}

func (h *vaultHeadRec) public(lane string) a2a.VaultHead {
	return a2a.VaultHead{Lane: lane, Version: h.Version, Root: h.Root, Refs: h.Refs, Device: h.Device, Updated: h.Updated}
}

// vaultBox is the in-memory side of one mailbox's vault. mu serialises every mutation of
// the box (uploads' final rename, head updates, collection) and every head read.
type vaultBox struct {
	mu          sync.Mutex
	loaded      bool
	bytes       int64
	blobs       int64
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

func (s *Server) vaultBoxDir(box string) string { return filepath.Join(s.dataDir, "vault", box) }

func (s *Server) vaultBlobPath(box, id string) string {
	return filepath.Join(s.vaultBoxDir(box), "blobs", id[:2], id)
}

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

// vaultLoadLocked scans box's blobs (usage) and reads its heads the first time. A mailbox
// without a vault directory loads as empty. Caller holds vb.mu.
func (s *Server) vaultLoadLocked(box string, vb *vaultBox) error {
	if vb.loaded {
		return nil
	}
	var nBytes, nBlobs int64
	err := filepath.WalkDir(filepath.Join(s.vaultBoxDir(box), "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !a2a.ValidVaultID(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		nBytes += info.Size()
		nBlobs++
		return nil
	})
	if err != nil {
		return fmt.Errorf("vault: scan blobs: %w", err)
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
	vb.bytes, vb.blobs, vb.heads, vb.loaded = nBytes, nBlobs, heads, true
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

// touch refreshes a blob's mtime (the garbage collector's "recently confirmed" clock).
func touch(p string) {
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// vaultPutBlob: PUT /vault/{box}/blob/{id}, body = raw blob bytes.
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
	dst := s.vaultBlobPath(box, id)
	quota := s.VaultQuota()

	vb := s.vaultOpen(w, box)
	if vb == nil {
		return
	}
	if fileExists(dst) { // idempotent: already stored
		touch(dst)
		vb.mu.Unlock()
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, a2a.MaxVaultBlobBytes+1))
		WriteJSON(w, 200, map[string]any{"ok": true, "existed": true})
		return
	}
	if r.ContentLength > 0 && vb.bytes+r.ContentLength > quota && !s.vaultReclaimLocked(box, vb, r.ContentLength, quota) {
		vb.mu.Unlock()
		WriteError(w, http.StatusRequestEntityTooLarge, a2a.VaultQuotaCode)
		return
	}
	vb.mu.Unlock()

	// Stream into tmp/ outside the lock (a 4 MiB body on a slow link takes a while).
	tmpDir := filepath.Join(s.vaultBoxDir(box), "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	f, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	tmp := f.Name()
	n, err := io.Copy(f, io.LimitReader(r.Body, a2a.MaxVaultBlobBytes+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		WriteError(w, 400, "failed to read blob body")
		return
	}
	if n > a2a.MaxVaultBlobBytes {
		_ = os.Remove(tmp)
		WriteError(w, http.StatusRequestEntityTooLarge, "blob exceeds 4 MiB")
		return
	}
	if n == 0 {
		_ = os.Remove(tmp)
		WriteError(w, 400, "empty blob")
		return
	}

	vb.mu.Lock()
	defer vb.mu.Unlock()
	if fileExists(dst) { // a concurrent upload of the same id won
		_ = os.Remove(tmp)
		touch(dst)
		WriteJSON(w, 200, map[string]any{"ok": true, "existed": true})
		return
	}
	if vb.bytes+n > quota && !s.vaultReclaimLocked(box, vb, n, quota) {
		_ = os.Remove(tmp)
		WriteError(w, http.StatusRequestEntityTooLarge, a2a.VaultQuotaCode)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		_ = os.Remove(tmp)
		WriteError(w, 500, err.Error())
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		WriteError(w, 500, err.Error())
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
	return vb.bytes+need <= quota
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
	f, err := os.Open(s.vaultBlobPath(box, id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			WriteError(w, 404, a2a.VaultNoBlobCode)
			return
		}
		WriteError(w, 500, err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		WriteError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(200)
	_, _ = io.Copy(w, f)
}

// vaultHas: POST /vault/{box}/has {"ids":[...]} -> {"missing":[...]} in request order.
// Present blobs get their grace clock refreshed, so a client that skips uploading them
// cannot lose them to a collection before it publishes the head that references them.
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
	vb := s.vaultBoxFor(box)
	vb.mu.Lock() // exclude a concurrent collection between "present" and the touch
	missing := []string{}
	for _, id := range body.IDs {
		p := s.vaultBlobPath(box, id)
		if fileExists(p) {
			touch(p)
		} else {
			missing = append(missing, id)
		}
	}
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
	// The version must be complete: root, refs and everything refs names are stored.
	var missing []string
	for _, id := range []string{body.Root, body.Refs} {
		if !fileExists(s.vaultBlobPath(box, id)) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		raw, err := os.ReadFile(s.vaultBlobPath(box, body.Refs))
		if err != nil {
			vb.mu.Unlock()
			WriteError(w, 500, err.Error())
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
			if !seen[id] && !fileExists(s.vaultBlobPath(box, id)) {
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
		if len(next.History) > a2a.VaultKeepVersions-1 {
			next.History = next.History[:a2a.VaultKeepVersions-1]
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
// An upload racing the purge fails (its temp file is gone) rather than resurrecting data.
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
	if err := os.RemoveAll(s.vaultBoxDir(box)); err != nil {
		vb.loaded = false // partially removed: rescan on next use instead of trusting the counters
		log.Printf("[relay] vault box=%s purge failed: %v", a2a.ShortFp(box), err)
		WriteError(w, 500, err.Error())
		return
	}
	vb.bytes, vb.blobs, vb.heads = 0, 0, map[string]*vaultHeadRec{}
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

// vaultGCLocked deletes every blob of box that no retained lane version references and
// that was not written / confirmed within vaultGrace, plus stale upload temp files.
// Caller holds vb.mu.
func (s *Server) vaultGCLocked(box string, vb *vaultBox) (int, error) {
	vb.lastGC = time.Now()
	if err := s.vaultLoadLocked(box, vb); err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for lane, h := range vb.heads {
		for _, rev := range append([]vaultRev{h.vaultRev}, h.History...) {
			live[rev.Root], live[rev.Refs] = true, true
			raw, err := os.ReadFile(s.vaultBlobPath(box, rev.Refs))
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
	cutoff := time.Now().Add(-s.vaultGrace)
	removed := 0
	blobsDir := filepath.Join(s.vaultBoxDir(box), "blobs")
	shards, _ := os.ReadDir(blobsDir)
	for _, sh := range shards {
		if !sh.IsDir() {
			continue
		}
		shardDir := filepath.Join(blobsDir, sh.Name())
		files, _ := os.ReadDir(shardDir)
		left := len(files)
		for _, f := range files {
			id := f.Name()
			if f.IsDir() || !a2a.ValidVaultID(id) || live[id] {
				continue
			}
			info, err := f.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(shardDir, id)) == nil {
				vb.bytes -= info.Size()
				vb.blobs--
				removed++
				left--
			}
		}
		if left == 0 {
			_ = os.Remove(shardDir) // only succeeds when empty
		}
	}
	tmpDir := filepath.Join(s.vaultBoxDir(box), "tmp")
	tmps, _ := os.ReadDir(tmpDir)
	for _, f := range tmps {
		if info, err := f.Info(); err == nil && !f.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(tmpDir, f.Name()))
		}
	}
	return removed, nil
}
