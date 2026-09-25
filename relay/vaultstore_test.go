package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// memStore is a VaultBlobStore in memory that counts its calls.
type memStore struct {
	mu      sync.Mutex
	blobs   map[string][]byte // box + "/" + id
	calls   map[string]int    // "put" / "get" / "delete" / "deletebox"
	putHook func(id string) error
}

func newMemStore() *memStore {
	return &memStore{blobs: map[string][]byte{}, calls: map[string]int{}}
}

func (m *memStore) Put(_ context.Context, box, id string, data []byte) error {
	m.mu.Lock()
	m.calls["put"]++
	hook := m.putHook
	m.mu.Unlock()
	if hook != nil {
		if err := hook(id); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[box+"/"+id] = append([]byte(nil), data...)
	return nil
}

func (m *memStore) Get(_ context.Context, box, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["get"]++
	b, ok := m.blobs[box+"/"+id]
	if !ok {
		return nil, ErrVaultBlobNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *memStore) Delete(_ context.Context, box, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["delete"]++
	delete(m.blobs, box+"/"+id)
	return nil
}

func (m *memStore) DeleteBox(_ context.Context, box string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["deletebox"]++
	for k := range m.blobs {
		if strings.HasPrefix(k, box+"/") {
			delete(m.blobs, k)
		}
	}
	return nil
}

func (m *memStore) snapshot() (map[string]int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := map[string]int{}
	for k, v := range m.calls {
		c[k] = v
	}
	return c, len(m.blobs)
}

func (m *memStore) has(box, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[box+"/"+id]
	return ok
}

func sameCalls(a, b map[string]int) bool {
	for _, k := range []string{"put", "get", "delete", "deletebox"} {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

// A foreign store receives exactly the calls the index cannot answer: has, usage and the
// completeness check of a head update never touch it.
func TestVaultStoreCallsAndIndex(t *testing.T) {
	f := newVaultFixture(t)
	ms := newMemStore()
	f.s.SetVaultBlobStore(ms)

	root, refs, ids := f.putVersion(t, "dev-A", "v1", "a", "b")
	if c, n := ms.snapshot(); c["put"] != 4 || n != 4 {
		t.Fatalf("four uploads, four store puts: %v (%d stored)", c, n)
	}
	if _, err := os.Stat(f.diskBlobPath(root)); !os.IsNotExist(err) {
		t.Fatalf("no blob may land on the relay's disk with a foreign store (err=%v)", err)
	}
	// Idempotent re-upload: answered from the index, no store write.
	before, _ := ms.snapshot()
	f.putBlob(t, "dev-A", []byte("a"))
	if after, _ := ms.snapshot(); !sameCalls(before, after) {
		t.Fatalf("idempotent put must not reach the store: %v -> %v", before, after)
	}
	// has and usage: index only.
	absent := blobID([]byte("absent"))
	code, body := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": []string{ids[0], absent, root}})
	if m, _ := body["missing"].([]any); code != 200 || len(m) != 1 || m[0] != absent {
		t.Fatalf("has: %d %v", code, body)
	}
	if u := f.usage(t); u.Blobs != 4 {
		t.Fatalf("usage: %+v", u)
	}
	// A blob the index does not know is 404 without asking the store.
	if code, _ := f.raw(t, "GET", f.vpath("/blob/"+absent), "", nil); code != 404 {
		t.Fatalf("unknown blob: want 404, got %d", code)
	}
	if after, _ := ms.snapshot(); !sameCalls(before, after) {
		t.Fatalf("has / usage / unknown GET must not reach the store: %v -> %v", before, after)
	}
	// Head update: one store read (the refs blob), nothing else.
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("head: %d %v", code, body)
	}
	after, _ := ms.snapshot()
	if after["get"] != before["get"]+1 || after["put"] != before["put"] || after["delete"] != before["delete"] {
		t.Fatalf("head update must read exactly the refs blob: %v -> %v", before, after)
	}
	// GET streams the store's bytes.
	if code, got := f.raw(t, "GET", f.vpath("/blob/"+ids[1]), "", nil); code != 200 || string(got) != "b" {
		t.Fatalf("get: %d %q", code, got)
	}

	// Quota: a refused upload never reaches the store.
	f.s.SetVaultQuota(f.usage(t).Bytes + 5)
	before, _ = ms.snapshot()
	big := bytes.Repeat([]byte("q"), 50)
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(big)), "", big); code != 413 {
		t.Fatalf("over quota: want 413, got %d", code)
	}
	if after, _ := ms.snapshot(); after["put"] != before["put"] {
		t.Fatalf("a refused upload must not be stored: %v -> %v", before, after)
	}
	f.s.SetVaultQuota(0)

	// Collection: only the unreferenced, expired blob is deleted, via the store.
	junk := f.putBlob(t, "", []byte("junk"))
	f.age(t, junk)
	for _, id := range append([]string{root, refs}, ids...) {
		f.age(t, id)
	}
	before, _ = ms.snapshot()
	if n, err := f.s.vaultGC(f.box); err != nil || n != 1 {
		t.Fatalf("gc: %d %v", n, err)
	}
	after, _ = ms.snapshot()
	if after["delete"] != before["delete"]+1 || ms.has(f.box, junk) || !ms.has(f.box, root) {
		t.Fatalf("gc must delete exactly the junk blob: %v -> %v", before, after)
	}
	if code, _ := f.raw(t, "GET", f.vpath("/blob/"+junk), "", nil); code != 404 {
		t.Fatalf("collected blob: want 404, got %d", code)
	}
	if u := f.usage(t); u.Blobs != 4 {
		t.Fatalf("usage after gc: %+v", u)
	}

	// Restart with the same store: the index is on disk, has still needs no store call.
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	s2.vaultGCDelay = -1
	s2.SetVaultBlobStore(ms)
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	f2 := &deviceFixture{s: s2, srv: srv2, id: f.id, box: f.box}
	before, _ = ms.snapshot()
	code, body = f2.do(t, "POST", f2.vpath("/has"), "", "", map[string]any{"ids": []string{root, refs, ids[0], ids[1], junk}})
	if m, _ := body["missing"].([]any); code != 200 || len(m) != 1 || m[0] != junk {
		t.Fatalf("has after restart: %d %v", code, body)
	}
	if u := f2.usage(t); u.Blobs != 4 {
		t.Fatalf("usage after restart: %+v", u)
	}
	if after, _ := ms.snapshot(); !sameCalls(before, after) {
		t.Fatalf("a restarted relay must answer has from its index: %v -> %v", before, after)
	}

	// Purge: one DeleteBox, the store is empty for this box, usage is zero.
	code, body = f2.do(t, "DELETE", f2.vpath(""), "", "dev-A", nil)
	if code != 200 || body["blobs"] != float64(4) {
		t.Fatalf("purge: %d %v", code, body)
	}
	if c, n := ms.snapshot(); c["deletebox"] != 1 || n != 0 {
		t.Fatalf("purge must clear the store: %v (%d left)", c, n)
	}
	if u := f2.usage(t); u.Blobs != 0 || u.Bytes != 0 {
		t.Fatalf("usage after purge: %+v", u)
	}
}

// A store failure leaves nothing indexed and releases the quota reservation.
func TestVaultStorePutFailure(t *testing.T) {
	f := newVaultFixture(t)
	ms := newMemStore()
	ms.putHook = func(string) error { return errors.New("store down") }
	f.s.SetVaultBlobStore(ms)
	data := []byte("will fail")
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(data)), "", data); code != 500 {
		t.Fatalf("store failure: want 500, got %d", code)
	}
	vb := f.s.vaultBoxFor(f.box)
	vb.mu.Lock()
	reserved, inflight, indexed := vb.reserved, len(vb.inflight), len(vb.index)
	vb.mu.Unlock()
	if reserved != 0 || inflight != 0 || indexed != 0 {
		t.Fatalf("failed upload left state behind: reserved=%d inflight=%d indexed=%d", reserved, inflight, indexed)
	}
	code, body := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": []string{blobID(data)}})
	if m, _ := body["missing"].([]any); code != 200 || len(m) != 1 {
		t.Fatalf("failed upload must read as missing: %d %v", code, body)
	}
	ms.putHook = nil
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(data)), "", data); code != 200 {
		t.Fatalf("retry after the store recovered: %d", code)
	}
}

// An upload still storing its bytes when the vault is purged does not come back.
func TestVaultUploadRacingPurge(t *testing.T) {
	f := newVaultFixture(t)
	ms := newMemStore()
	f.s.SetVaultBlobStore(ms)
	f.putBlob(t, "dev-A", []byte("before")) // dev-A claims the mailbox, so it may purge
	entered, release := make(chan struct{}), make(chan struct{})
	ms.putHook = func(string) error {
		close(entered)
		<-release
		return nil
	}
	data := []byte("in flight during the purge")
	done := make(chan int, 1)
	go func() {
		code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(data)), "dev-A", data)
		done <- code
	}()
	<-entered
	if code, body := f.do(t, "DELETE", f.vpath(""), "", "dev-A", nil); code != 200 {
		t.Fatalf("purge: %d %v", code, body)
	}
	close(release)
	if code := <-done; code != 500 {
		t.Fatalf("an upload racing a purge must fail, got %d", code)
	}
	if ms.has(f.box, blobID(data)) {
		t.Fatal("the racing upload's bytes must be dropped from the store")
	}
	if u := f.usage(t); u.Blobs != 0 || u.Bytes != 0 {
		t.Fatalf("usage after the race: %+v", u)
	}
}

// A data directory written before the index existed (blobs on disk, no index.jsonl) is
// indexed by one scan on first use and keeps working, including across restarts.
func TestVaultIndexRebuiltFromLegacyDisk(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, ids := f.putVersion(t, "dev-A", "v1", "a", "b")
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("head: %d %v", code, body)
	}
	idxPath := f.s.vaultIndexPath(f.box)
	if err := os.Remove(idxPath); err != nil {
		t.Fatal(err)
	}
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	s2.vaultGCDelay = -1
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	f2 := &deviceFixture{s: s2, srv: srv2, id: f.id, box: f.box}
	if u := f2.usage(t); u.Blobs != 4 || u.Bytes != int64(len("a")+len("b")+len("manifest v1")+len(a2a.EncodeVaultRefs(ids))) {
		t.Fatalf("usage from the rebuilt index: %+v", u)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("the rebuilt index must be written: %v", err)
	}
	code, body := f2.do(t, "POST", f2.vpath("/has"), "", "", map[string]any{"ids": append([]string{root, refs}, ids...)})
	if m, _ := body["missing"].([]any); code != 200 || len(m) != 0 {
		t.Fatalf("has after rebuild: %d %v", code, body)
	}
}

// A torn last line (crash mid-append) is dropped and the index compacted; a corrupt line
// in the middle refuses to load rather than forget blobs.
func TestVaultIndexTornTail(t *testing.T) {
	f := newVaultFixture(t)
	a := f.putBlob(t, "", []byte("a"))
	b := f.putBlob(t, "", []byte("b"))
	idxPath := f.s.vaultIndexPath(f.box)
	fh, err := os.OpenFile(idxPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString(`{"put":"` + strings.Repeat("c", 64) + `","si`)
	fh.Close()

	s2, _ := New(f.s.DataDir())
	vb := s2.vaultBoxFor(f.box)
	vb.mu.Lock()
	err = s2.vaultLoadLocked(f.box, vb)
	_, okA := vb.index[a]
	_, okB := vb.index[b]
	n := len(vb.index)
	vb.mu.Unlock()
	if err != nil || !okA || !okB || n != 2 {
		t.Fatalf("torn tail: err=%v index=%d a=%v b=%v", err, n, okA, okB)
	}
	raw, _ := os.ReadFile(idxPath)
	if !bytes.HasSuffix(raw, []byte("\n")) || bytes.Contains(raw, []byte(strings.Repeat("c", 64))) {
		t.Fatalf("the torn line must be compacted away: %q", raw)
	}

	// Corrupt a middle line.
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	lines[1] = "{not json"
	_ = os.WriteFile(idxPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	s3, _ := New(f.s.DataDir())
	vb3 := s3.vaultBoxFor(f.box)
	vb3.mu.Lock()
	err = s3.vaultLoadLocked(f.box, vb3)
	vb3.mu.Unlock()
	if err == nil {
		t.Fatal("a corrupt index line must refuse to load")
	}
}

// The index log is compacted once it outgrows the live entries.
func TestVaultIndexCompaction(t *testing.T) {
	f := newVaultFixture(t)
	f.s.vaultIndexSlack = 8
	a := f.putBlob(t, "", []byte("a"))
	vb := f.s.vaultBoxFor(f.box)
	vb.mu.Lock()
	for i := 0; i < 50; i++ {
		f.s.vaultTouchLocked(f.box, vb, []string{a})
	}
	lines := vb.indexLines
	vb.mu.Unlock()
	if lines > 2+8+1 {
		t.Fatalf("the index log should have been compacted, %d lines", lines)
	}
	idx, n, _, err := readVaultIndex(f.s.vaultIndexPath(f.box))
	if err != nil || len(idx) != 1 || n != lines {
		t.Fatalf("replayed index: %d entries, %d lines (want %d), err=%v", len(idx), n, lines, err)
	}
}

// Blobs a disk-backed relay stored move into a new store with MigrateVaultBlobs; before
// that, the mailbox refuses to serve rather than claim blobs the new store lacks.
func TestVaultMigrateDiskToStore(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, ids := f.putVersion(t, "dev-A", "v1", "a", "b")
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("head: %d %v", code, body)
	}
	before := f.usage(t)
	// Simulate the pre-index layout for good measure: the migration rebuilds the index too.
	_ = os.Remove(f.s.vaultIndexPath(f.box))

	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	s2.vaultGCDelay = -1
	ms := newMemStore()
	s2.SetVaultBlobStore(ms)
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	f2 := &deviceFixture{s: s2, srv: srv2, id: f.id, box: f.box}

	if code, _ := f2.raw(t, "GET", f2.vpath("/blob/"+ids[0]), "dev-A", nil); code != 500 {
		t.Fatalf("unmigrated blobs behind a foreign store: want 500, got %d", code)
	}
	var lines []string
	res, err := s2.MigrateVaultBlobs(context.Background(), func(format string, args ...any) {
		lines = append(lines, format)
	})
	if err != nil || res.Boxes != 1 || res.Blobs != 4 || res.Bytes != before.Bytes || len(lines) != 1 {
		t.Fatalf("migration: %+v err=%v log=%v", res, err, lines)
	}
	if _, err := os.Stat(filepath.Join(s2.vaultBoxDir(f.box), "blobs")); !os.IsNotExist(err) {
		t.Fatalf("local blobs must be gone after the migration (err=%v)", err)
	}
	if _, n := ms.snapshot(); n != 4 {
		t.Fatalf("store holds %d blobs, want 4", n)
	}
	if u := f2.usage(t); u.Bytes != before.Bytes || u.Blobs != before.Blobs {
		t.Fatalf("usage after migration: before %+v after %+v", before, f2.usage(t))
	}
	if code, got := f2.raw(t, "GET", f2.vpath("/blob/"+ids[0]), "dev-A", nil); code != 200 || string(got) != "a" {
		t.Fatalf("blob after migration: %d %q", code, got)
	}
	code, raw := f2.raw(t, "GET", f2.vpath("/head/main"), "dev-A", nil)
	if code != 200 || !strings.Contains(string(raw), root) {
		t.Fatalf("head after migration: %d %s", code, raw)
	}
	// Idempotent: nothing left to move.
	if res, err := s2.MigrateVaultBlobs(context.Background(), nil); err != nil || res.Blobs != 0 || res.Boxes != 0 {
		t.Fatalf("second migration: %+v %v", res, err)
	}
	// And a disk-backed relay has nothing to migrate.
	if res, err := f.s.MigrateVaultBlobs(context.Background(), nil); err != nil || res.Blobs != 0 {
		t.Fatalf("disk store migration must be a no-op: %+v %v", res, err)
	}
}

// The number of retained versions per lane is configurable; lowering it releases the
// blobs of the versions that fell out at the next collection.
func TestVaultKeepVersionsConfigurable(t *testing.T) {
	f := newVaultFixture(t)
	if f.s.VaultKeepVersions() != a2a.VaultKeepVersions {
		t.Fatalf("default keep: %d", f.s.VaultKeepVersions())
	}
	var roots []string
	for v := 1; v <= 3; v++ {
		root, refs, _ := f.putVersion(t, "", fmt.Sprint("v", v), fmt.Sprint("only-in-v", v))
		if code, body := f.setHead(t, "", "main", int64(v-1), root, refs); code != 200 {
			t.Fatalf("v%d: %d %v", v, code, body)
		}
		roots = append(roots, root)
	}
	vb := f.s.vaultBoxFor(f.box)
	vb.mu.Lock()
	for id := range vb.index {
		m := vb.index[id]
		m.At = time.Now().Add(-vaultGrace - time.Hour).Unix()
		vb.index[id] = m
	}
	vb.mu.Unlock()
	// Default 3: all three versions survive.
	if n, err := f.s.vaultGC(f.box); err != nil || n != 0 {
		t.Fatalf("gc with keep=3: %d %v", n, err)
	}
	// Keep 2: v1 (root, refs, its own content) goes, v2 and v3 stay.
	f.s.SetVaultKeepVersions(2)
	if n, err := f.s.vaultGC(f.box); err != nil || n != 3 {
		t.Fatalf("gc with keep=2: want 3 blobs removed, got %d %v", n, err)
	}
	if f.stored(roots[0]) || !f.stored(roots[1]) || !f.stored(roots[2]) {
		t.Fatal("keep=2 must drop v1 and retain v2, v3")
	}
	// The next update trims the head record to keep-1 older versions.
	root4, refs4, _ := f.putVersion(t, "", "v4", "only-in-v4")
	if code, body := f.setHead(t, "", "main", 3, root4, refs4); code != 200 {
		t.Fatalf("v4: %d %v", code, body)
	}
	vb.mu.Lock()
	hist := len(vb.heads["main"].History)
	vb.mu.Unlock()
	if hist != 1 {
		t.Fatalf("history after update with keep=2: %d, want 1", hist)
	}
	f.s.SetVaultKeepVersions(0)
	if f.s.VaultKeepVersions() != a2a.VaultKeepVersions {
		t.Fatal("<= 0 restores the default")
	}
}
