// Capability directory sub-module of package relay (module A): isolated from the dumb-pipe post office logic.
// Stores only self-signed capability cards the user opted in to publish, with an inverted tag index for coarse filtering.
// Never touches private mail, does not know who knows whom; persisted as directory/<fingerprint>.json.
package relay

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/startupworld-ai/soulnet/a2a"
)

// DirEntry is one directory entry: the public Card (public keys / post office needed to contact) + the capability Profile.
type DirEntry struct {
	Card    *a2a.Card    `json:"card"`
	Profile *a2a.Profile `json:"profile"`
}

// Directory is the capability directory: in-memory inverted index + on-disk files.
type Directory struct {
	dir     string
	mu      sync.RWMutex
	entries map[string]*DirEntry // fingerprint -> entry

	// afterPublish is called after a successful HTTP listing (event hook of the CL reward engine, wired in relay.New).
	// nil-safe; the callback is best-effort and never affects the listing itself.
	afterPublish func(fp string)
}

// NewDirectory creates the directory and restores published entries from disk.
func NewDirectory(dataDir string) *Directory {
	d := &Directory{dir: filepath.Join(dataDir, "directory"), entries: map[string]*DirEntry{}}
	d.load()
	return d
}

func (d *Directory) load() {
	files, _ := os.ReadDir(d.dir)
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.dir, f.Name()))
		if err != nil {
			continue
		}
		var e DirEntry
		if json.Unmarshal(raw, &e) == nil && e.Profile != nil {
			d.entries[e.Profile.Fingerprint] = &e
		}
	}
}

func (d *Directory) path(fp string) (string, error) {
	if fp == "" || strings.ContainsAny(fp, `/\`) || strings.Contains(fp, "..") {
		return "", fmt.Errorf("非法指纹")
	}
	return filepath.Join(d.dir, fp+".json"), nil
}

// verifyEntry checks that an entry is self-consistent: card self-signed, profile self-signed, both fingerprints match.
func verifyEntry(e *DirEntry) error {
	if e == nil || e.Card == nil || e.Profile == nil {
		return fmt.Errorf("条目缺 card 或 profile")
	}
	if err := e.Card.Verify(); err != nil {
		return fmt.Errorf("card 校验失败: %w", err)
	}
	if err := e.Profile.Verify(e.Card.EdPub); err != nil {
		return fmt.Errorf("profile 校验失败: %w", err)
	}
	cfp, err := e.Card.Fingerprint()
	if err != nil || cfp != e.Profile.Fingerprint {
		return fmt.Errorf("card 与 profile 指纹不符")
	}
	return nil
}

// Publish lists an entry after verification (same fingerprint overwrites).
func (d *Directory) Publish(e *DirEntry) error {
	if err := verifyEntry(e); err != nil {
		return err
	}
	p, err := d.path(e.Profile.Fingerprint)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(e, "", "  ")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return err
	}
	d.mu.Lock()
	d.entries[e.Profile.Fingerprint] = e
	d.mu.Unlock()
	return nil
}

// Unpublish delists (deletes file + memory). A missing entry counts as success.
func (d *Directory) Unpublish(fp string) error {
	p, err := d.path(fp)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	d.mu.Lock()
	delete(d.entries, fp)
	d.mu.Unlock()
	return nil
}

// Query does a coarse inverted-index search: ranks by tag hits + keyword hits in title/desc/intro, returns the top limit in descending order.
func (d *Directory) Query(tags []string, keyword string, limit int) []*DirEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	want := map[string]bool{}
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}
	kw := strings.TrimSpace(keyword)
	type scored struct {
		e *DirEntry
		s int
	}
	var out []scored
	for _, e := range d.entries {
		score := 0
		for _, sk := range e.Profile.Skills {
			for _, tg := range sk.Tags {
				if want[tg] {
					score += 2
				}
			}
			if kw != "" && (strings.Contains(sk.Title, kw) || strings.Contains(sk.Desc, kw)) {
				score++
			}
		}
		if kw != "" && strings.Contains(e.Profile.Intro, kw) {
			score++
		}
		// no search criteria = list everything; with criteria, only hits
		if (len(want) == 0 && kw == "") || score > 0 {
			out = append(out, scored{e, score})
		}
	}
	// Deterministic ordering: candidates come from map iteration (random order), so a TOTAL order comparator is required,
	// otherwise equal-score entries keep a random map order and swap places on every refresh/restart.
	// Sort contract: score desc -> updated time desc (newest first) -> fingerprint asc (unique tie-breaker).
	sort.Slice(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		ti, tj := out[i].e.Profile.UpdatedAt, out[j].e.Profile.UpdatedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].e.Profile.Fingerprint < out[j].e.Profile.Fingerprint
	})
	res := make([]*DirEntry, 0, len(out))
	for _, x := range out {
		if limit > 0 && len(res) >= limit {
			break
		}
		res = append(res, x.e)
	}
	return res
}

// --- HTTP handlers ---

func (d *Directory) handlePublish(w http.ResponseWriter, r *http.Request) {
	var e DirEntry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "请求体需为 {card, profile}", 400)
		return
	}
	if err := d.Publish(&e); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if d.afterPublish != nil {
		d.afterPublish(e.Profile.Fingerprint) // CL reward event (the first-listing bonus is decided by the engine idempotency key)
	}
	writeDirJSON(w, map[string]any{"ok": true})
}

// handleUnpublish POST /directory/unpublish -- body {fingerprint, ed_pub, sig}.
// Signature REQUIRED: delisting is destructive, only the card owner may delist their own card.
func (d *Directory) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Fingerprint string `json:"fingerprint"`
		EdPub       string `json:"ed_pub"`
		Sig         string `json:"sig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "请求体需为 {fingerprint, ed_pub, sig}", 400)
		return
	}
	// SIGNATURE REQUIRED: the listing side (verifyEntry) checks card/profile self-signatures + fingerprint consistency, but the
	// delisting side had no gate at all -- and fingerprint is a field publicly returned by GET /directory/query,
	// so a loop over the query results could wipe the entire discovery square.
	// Same origin and shape as the app-square delisting hole fixed in 4353a84a; the capability directory was missed back then.
	//
	// Fixed the same way as the app square: the owner signs "directory-unpublish:<fingerprint>" with their private key,
	// the relay verifies with the public key and checks that this key's fingerprint is the one being delisted --
	// a valid signature with someone else's fingerprint is rejected.
	if err := verifyDirUnpublish(b.Fingerprint, b.EdPub, b.Sig); err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	if err := d.Unpublish(b.Fingerprint); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeDirJSON(w, map[string]any{"ok": true})
}

// verifyDirUnpublish checks "this person is allowed to delist this card".
// Both must hold: the signature matches the public key, and that key's fingerprint is the one being delisted.
//
// Bytes to sign come from a2a.DirUnpublishSigningBytes (relay already imports a2a),
// shared with the client instead of two copies -- a mismatch fails verification immediately rather than silently passing.
func verifyDirUnpublish(fp, edPubB64, sigB64 string) error {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return fmt.Errorf("缺 fingerprint")
	}
	if strings.TrimSpace(edPubB64) == "" || strings.TrimSpace(sigB64) == "" {
		// Old clients (before 3.8) only send a bare {fingerprint} and land here. No compatibility window:
		// "delete on unsigned request and just log it" would leave the hole open; a grace period has zero security value.
		return fmt.Errorf("下架需带 ed_pub + sig 签名（客户端版本过低，请升级灵镜后再下架）")
	}
	pub, err := a2a.DecodeKey(edPubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("公钥非法")
	}
	if a2a.Fingerprint(ed25519.PublicKey(pub)) != fp {
		return fmt.Errorf("只有名片主人本人能下架它")
	}
	sig, err := a2a.DecodeKey(sigB64)
	if err != nil {
		return fmt.Errorf("下架签名非 base64")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), a2a.DirUnpublishSigningBytes(fp), sig) {
		return fmt.Errorf("下架签名校验失败")
	}
	return nil
}

// Get fetches exactly one directory entry by fingerprint (nil if missing).
func (d *Directory) Get(fp string) *DirEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.entries[fp]
}

// handleFetch GET /directory/fetch?fp=<fingerprint> -- fetch exactly one public card by fingerprint.
// Used after adding a friend to sync their full capability card locally (Query is a coarse tag/kw search and cannot target a single fingerprint).
func (d *Directory) handleFetch(w http.ResponseWriter, r *http.Request) {
	fp := strings.TrimSpace(r.URL.Query().Get("fp"))
	if fp == "" {
		http.Error(w, "missing fp parameter", 400)
		return
	}
	e := d.Get(fp)
	if e == nil {
		http.Error(w, "no public card for this fingerprint", 404)
		return
	}
	writeDirJSON(w, e)
}

func (d *Directory) handleQuery(w http.ResponseWriter, r *http.Request) {
	var tags []string
	if t := r.URL.Query().Get("tags"); t != "" {
		tags = strings.Split(t, ",")
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeDirJSON(w, map[string]any{"entries": d.Query(tags, r.URL.Query().Get("kw"), limit)})
}

func writeDirJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
