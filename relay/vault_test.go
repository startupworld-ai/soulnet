package relay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// newVaultFixture is a device fixture with automatic collection disabled (tests drive
// vaultGC themselves, so a timer never races their assertions).
func newVaultFixture(t *testing.T) *deviceFixture {
	t.Helper()
	f := newDeviceFixture(t)
	f.s.vaultGCDelay = -1
	return f
}

// blobID is the test stand-in for the client's keyed hash.
func blobID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// raw sends an owner-signed request with a raw body and returns status + body bytes.
func (f *deviceFixture) raw(t *testing.T, method, path, device string, body []byte) (int, []byte) {
	t.Helper()
	priv, err := f.id.EdPrivate()
	if err != nil {
		t.Fatal(err)
	}
	req := signedReq(t, priv, method, f.srv.URL+path, path, body)
	if device != "" {
		req.Header.Set(a2a.HeaderDevice, device)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (f *deviceFixture) vpath(suffix string) string { return "/vault/" + f.box + suffix }

// putBlob uploads data and fails the test unless the relay answers 200.
func (f *deviceFixture) putBlob(t *testing.T, device string, data []byte) string {
	t.Helper()
	id := blobID(data)
	if code, body := f.raw(t, "PUT", f.vpath("/blob/"+id), device, data); code != 200 {
		t.Fatalf("put blob: %d %s", code, body)
	}
	return id
}

// putVersion uploads the given content blobs, a root and a refs blob naming them, and
// returns (root, refs, content ids).
func (f *deviceFixture) putVersion(t *testing.T, device, tag string, contents ...string) (string, string, []string) {
	t.Helper()
	var ids []string
	for _, c := range contents {
		ids = append(ids, f.putBlob(t, device, []byte(c)))
	}
	root := f.putBlob(t, device, []byte("manifest "+tag))
	refs := f.putBlob(t, device, a2a.EncodeVaultRefs(ids))
	return root, refs, ids
}

func (f *deviceFixture) setHead(t *testing.T, device, lane string, prev int64, root, refs string) (int, map[string]any) {
	t.Helper()
	return f.do(t, "PUT", f.vpath("/head/"+lane), "", device, map[string]any{"prev_version": prev, "root": root, "refs": refs})
}

func (f *deviceFixture) usage(t *testing.T) a2a.VaultUsage {
	t.Helper()
	code, body := f.raw(t, "GET", f.vpath("/usage"), "", nil)
	if code != 200 {
		t.Fatalf("usage: %d %s", code, body)
	}
	var u a2a.VaultUsage
	if err := json.Unmarshal(body, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// age backdates a blob past the collection grace period.
func (f *deviceFixture) age(t *testing.T, id string) {
	t.Helper()
	old := time.Now().Add(-vaultGrace - time.Hour)
	if err := os.Chtimes(f.s.vaultBlobPath(f.box, id), old, old); err != nil {
		t.Fatal(err)
	}
}

func (f *deviceFixture) stored(id string) bool { return fileExists(f.s.vaultBlobPath(f.box, id)) }

func TestVaultRequiresOwnerSignature(t *testing.T) {
	f := newVaultFixture(t)
	id := f.putBlob(t, "", []byte("secret"))
	other, err := a2a.NewIdentity(t.TempDir(), "other", []string{f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	otherPriv, _ := other.EdPrivate()
	routes := []struct{ method, path string }{
		{"PUT", f.vpath("/blob/" + id)},
		{"GET", f.vpath("/blob/" + id)},
		{"POST", f.vpath("/has")},
		{"GET", f.vpath("/head/main")},
		{"PUT", f.vpath("/head/main")},
		{"DELETE", f.vpath("/head/main") + "?prev_version=1"},
		{"GET", f.vpath("/heads")},
		{"GET", f.vpath("/usage")},
		{"DELETE", f.vpath("")},
	}
	for _, rt := range routes {
		body := []byte(`{"ids":[],"prev_version":0,"root":"` + id + `","refs":"` + id + `"}`)
		// Unsigned.
		req, _ := http.NewRequest(rt.method, f.srv.URL+rt.path, bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s %s unsigned: want 401, got %d", rt.method, rt.path, resp.StatusCode)
		}
		// Signed by another identity (a valid signature, but not the mailbox owner).
		signPath, _, _ := strings.Cut(rt.path, "?")
		req = signedReq(t, otherPriv, rt.method, f.srv.URL+rt.path, signPath, body)
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s %s by a stranger: want 401, got %d", rt.method, rt.path, resp.StatusCode)
		}
	}
}

func TestVaultBlobPutIsIdempotentAndCounted(t *testing.T) {
	f := newVaultFixture(t)
	data := []byte("ciphertext-1")
	id := blobID(data)
	code, body := f.raw(t, "PUT", f.vpath("/blob/"+id), "", data)
	if code != 200 || !strings.Contains(string(body), `"existed":false`) {
		t.Fatalf("first put: %d %s", code, body)
	}
	code, body = f.raw(t, "PUT", f.vpath("/blob/"+id), "", data)
	if code != 200 || !strings.Contains(string(body), `"existed":true`) {
		t.Fatalf("second put must be idempotent: %d %s", code, body)
	}
	if u := f.usage(t); u.Blobs != 1 || u.Bytes != int64(len(data)) || u.Quota != DefaultVaultQuota {
		t.Fatalf("usage must count the blob once: %+v", u)
	}
	code, got := f.raw(t, "GET", f.vpath("/blob/"+id), "", nil)
	if code != 200 || !bytes.Equal(got, data) {
		t.Fatalf("get: %d %q", code, got)
	}
	// Malformed id, empty body, oversize body.
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/ABC"), "", data); code != 400 {
		t.Fatalf("bad id: want 400, got %d", code)
	}
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+strings.Repeat("0", 64)), "", nil); code != 400 {
		t.Fatalf("empty blob: want 400, got %d", code)
	}
	big := make([]byte, a2a.MaxVaultBlobBytes+1)
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(big)), "", big); code != 413 {
		t.Fatalf("oversize blob: want 413, got %d", code)
	}
	if u := f.usage(t); u.Blobs != 1 {
		t.Fatalf("refused uploads must not count: %+v", u)
	}
}

func TestVaultHasAndEmptyMailbox(t *testing.T) {
	f := newVaultFixture(t)
	// A mailbox that never used the vault: 404 / empty, never 500.
	if code, body := f.raw(t, "GET", f.vpath("/head/main"), "", nil); code != 404 || !strings.Contains(string(body), a2a.VaultNoLaneCode) {
		t.Fatalf("head of an empty vault: %d %s", code, body)
	}
	if code, body := f.raw(t, "GET", f.vpath("/blob/"+strings.Repeat("a", 64)), "", nil); code != 404 || !strings.Contains(string(body), a2a.VaultNoBlobCode) {
		t.Fatalf("blob of an empty vault: %d %s", code, body)
	}
	if code, body := f.raw(t, "GET", f.vpath("/heads"), "", nil); code != 200 || !strings.Contains(string(body), `"heads":[]`) {
		t.Fatalf("heads of an empty vault: %d %s", code, body)
	}
	if u := f.usage(t); u.Blobs != 0 || u.Bytes != 0 {
		t.Fatalf("usage of an empty vault: %+v", u)
	}
	if _, err := os.Stat(f.s.vaultBoxDir(f.box)); !os.IsNotExist(err) {
		t.Fatalf("read-only calls must not create the vault directory (err=%v)", err)
	}

	present := f.putBlob(t, "", []byte("present"))
	absent1, absent2 := blobID([]byte("absent-1")), blobID([]byte("absent-2"))
	code, body := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": []string{absent1, present, absent2}})
	if code != 200 {
		t.Fatalf("has: %d %v", code, body)
	}
	missing, _ := body["missing"].([]any)
	if len(missing) != 2 || missing[0] != absent1 || missing[1] != absent2 {
		t.Fatalf("has must list the missing ids in request order: %v", body)
	}
	if code, _ := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": []string{"nope"}}); code != 400 {
		t.Fatalf("has with a malformed id: want 400, got %d", code)
	}
	tooMany := make([]string, a2a.MaxVaultHasIDs+1)
	for i := range tooMany {
		tooMany[i] = present
	}
	if code, _ := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": tooMany}); code != 400 {
		t.Fatalf("has over the id cap: want 400, got %d", code)
	}
}

func TestVaultHeadCompareAndSwap(t *testing.T) {
	f := newVaultFixture(t)
	root1, refs1, _ := f.putVersion(t, "dev-A", "v1", "a", "b")
	code, body := f.setHead(t, "dev-A", "main", 0, root1, refs1)
	if code != 200 {
		t.Fatalf("create main: %d %v", code, body)
	}
	head, _ := body["head"].(map[string]any)
	if head["version"] != float64(1) || head["root"] != root1 || head["device"] != "dev-A" || head["lane"] != "main" {
		t.Fatalf("new head: %v", head)
	}
	// A stale writer (still at version 0) loses.
	root2, refs2, _ := f.putVersion(t, "dev-A", "v2", "c")
	code, body = f.setHead(t, "dev-A", "main", 0, root2, refs2)
	if code != 409 || body["error"] != a2a.VaultConflictCode || body["version"] != float64(1) || body["root"] != root1 {
		t.Fatalf("stale prev_version: want 409 conflict at version 1, got %d %v", code, body)
	}
	if code, body = f.setHead(t, "dev-A", "main", 1, root2, refs2); code != 200 {
		t.Fatalf("advance to v2: %d %v", code, body)
	}
	code, got := f.raw(t, "GET", f.vpath("/head/main"), "dev-A", nil)
	var h a2a.VaultHead
	_ = json.Unmarshal(got, &h)
	if code != 200 || h.Version != 2 || h.Root != root2 || h.Refs != refs2 || h.Updated.IsZero() {
		t.Fatalf("GET head after v2: %d %s", code, got)
	}
	// Bad lane names.
	for _, lane := range []string{"other", "dev-", "dev-a%2Fb", "MAIN"} {
		if code, _ := f.setHead(t, "dev-A", lane, 0, root2, refs2); code != 400 && code != 404 {
			t.Fatalf("lane %q must be refused, got %d", lane, code)
		}
	}
}

func TestVaultHeadRequiresEveryReferencedBlob(t *testing.T) {
	f := newVaultFixture(t)
	root := f.putBlob(t, "", []byte("manifest"))
	ghost := blobID([]byte("never uploaded"))
	refs := f.putBlob(t, "", a2a.EncodeVaultRefs([]string{f.putBlob(t, "", []byte("x")), ghost}))
	code, body := f.setHead(t, "", "main", 0, root, refs)
	if code != 422 || body["error"] != a2a.VaultMissingCode || body["count"] != float64(1) {
		t.Fatalf("head naming a missing blob: want 422 missing, got %d %v", code, body)
	}
	if m, _ := body["missing"].([]any); len(m) != 1 || m[0] != ghost {
		t.Fatalf("422 must list the missing id: %v", body)
	}
	// Root or refs themselves not stored.
	if code, body = f.setHead(t, "", "main", 0, ghost, refs); code != 422 {
		t.Fatalf("missing root: want 422, got %d %v", code, body)
	}
	// A refs blob that is not a list of ids (e.g. the client encrypted it by mistake).
	notRefs := f.putBlob(t, "", []byte("\x00\x01 ciphertext, not a refs list"))
	if code, body = f.setHead(t, "", "main", 0, root, notRefs); code != 422 || body["error"] != a2a.VaultBadRefsCode {
		t.Fatalf("unparseable refs: want 422, got %d %v", code, body)
	}
	if code, _ := f.raw(t, "GET", f.vpath("/head/main"), "", nil); code != 404 {
		t.Fatalf("no refused update may create the lane, got %d", code)
	}
}

func TestVaultMainLaneOnlyActiveDevice(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, _ := f.putVersion(t, "dev-A", "v1", "a")
	// First device to show up with a header implicitly claims the mailbox (same rule as mail).
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("dev-A creates main: %d %v", code, body)
	}
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-A" {
		t.Fatalf("dev-A should now be active: %+v", ad)
	}
	// Any device of the identity may upload blobs...
	root2, refs2, _ := f.putVersion(t, "dev-B", "v2", "b")
	// ...but a non-active one may not advance main.
	code, body := f.setHead(t, "dev-B", "main", 1, root2, refs2)
	assertKicked(t, code, body, "dev-A")
	// Nor may a legacy client once a device holds the mailbox.
	code, body = f.setHead(t, "", "main", 1, root2, refs2)
	assertKicked(t, code, body, "dev-A")
	// dev-B parks its changes in its own lane...
	if code, body = f.setHead(t, "dev-B", "dev-dev-B", 0, root2, refs2); code != 200 {
		t.Fatalf("dev-B writes its own lane: %d %v", code, body)
	}
	// ...and may not touch anyone else's.
	if code, body = f.setHead(t, "dev-B", "dev-dev-A", 0, root2, refs2); code != 403 {
		t.Fatalf("dev-B on dev-A's lane: want 403, got %d %v", code, body)
	}
	if code, body = f.setHead(t, "dev-A", "dev-dev-B", 1, root2, refs2); code != 403 {
		t.Fatalf("even the active device may not advance another device's lane: want 403, got %d %v", code, body)
	}
	// Reads are open to every device of the identity.
	code, raw := f.raw(t, "GET", f.vpath("/heads"), "dev-B", nil)
	var out struct {
		Heads []a2a.VaultHead `json:"heads"`
	}
	_ = json.Unmarshal(raw, &out)
	if code != 200 || len(out.Heads) != 2 || out.Heads[0].Lane != "dev-dev-B" || out.Heads[1].Lane != "main" || out.Heads[0].Device != "dev-B" {
		t.Fatalf("heads: %d %s", code, raw)
	}
	// After a takeover dev-B is the one that may advance main.
	if code, _ := f.claim(t, "dev-B", "Phone"); code != 200 {
		t.Fatalf("claim: %d", code)
	}
	if code, body = f.setHead(t, "dev-B", "main", 1, root2, refs2); code != 200 {
		t.Fatalf("new active device advances main: %d %v", code, body)
	}
}

func TestVaultDeleteHead(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, _ := f.putVersion(t, "dev-A", "v1", "a")
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("main: %d %v", code, body)
	}
	if code, body := f.setHead(t, "dev-B", "dev-dev-B", 0, root, refs); code != 200 {
		t.Fatalf("dev lane: %d %v", code, body)
	}
	del := func(device, lane, prev string) (int, map[string]any) {
		return f.do(t, "DELETE", f.vpath("/head/"+lane), "?prev_version="+prev, device, nil)
	}
	if code, body := del("dev-A", "dev-dev-B", "2"); code != 409 || body["error"] != a2a.VaultConflictCode {
		t.Fatalf("delete at the wrong version: want 409 conflict, got %d %v", code, body)
	}
	if code, _ := del("dev-C", "dev-dev-B", "1"); code != 409 {
		t.Fatalf("a third, non-active device may not drop dev-B's lane: want 409 kicked, got %d", code)
	}
	if code, body := del("dev-A", "dev-dev-B", "1"); code != 200 {
		t.Fatalf("the active device drops a merged dev lane: %d %v", code, body)
	}
	if code, _ := del("dev-A", "dev-dev-B", "1"); code != 404 {
		t.Fatalf("second delete: want 404, got %d", code)
	}
	if code, body := del("dev-B", "main", "1"); code != 409 || body["error"] != a2a.KickedErrorCode {
		t.Fatalf("non-active device deleting main: want kicked, got %d %v", code, body)
	}
	if code, _ := del("dev-A", "main", ""); code != 400 {
		t.Fatalf("delete without prev_version: want 400, got %d", code)
	}
}

func TestVaultQuota(t *testing.T) {
	f := newVaultFixture(t)
	f.s.SetVaultQuota(100)
	f.putBlob(t, "", bytes.Repeat([]byte("a"), 60))
	data := bytes.Repeat([]byte("b"), 60)
	code, body := f.raw(t, "PUT", f.vpath("/blob/"+blobID(data)), "", data)
	if code != 413 || !strings.Contains(string(body), a2a.VaultQuotaCode) {
		t.Fatalf("over quota: want 413 %q, got %d %s", a2a.VaultQuotaCode, code, body)
	}
	if u := f.usage(t); u.Bytes != 60 || u.Blobs != 1 || u.Quota != 100 {
		t.Fatalf("a refused upload must not count: %+v", u)
	}
	// Re-uploading a blob that is already stored never trips the quota.
	if code, _ := f.raw(t, "PUT", f.vpath("/blob/"+blobID(bytes.Repeat([]byte("a"), 60))), "", bytes.Repeat([]byte("a"), 60)); code != 200 {
		t.Fatalf("idempotent re-upload at the quota: %d", code)
	}
	for _, v := range []struct {
		in   string
		want int64
	}{{"10G", 10 << 30}, {"10GiB", 10 << 30}, {"10gb", 10 << 30}, {"500M", 500 << 20}, {"1024", 1024}, {"4k", 4096}} {
		got, err := ParseByteSize(v.in)
		if err != nil || got != v.want {
			t.Fatalf("ParseByteSize(%q) = %d, %v; want %d", v.in, got, err, v.want)
		}
	}
	for _, bad := range []string{"", "0", "-1G", "1.5G", "ten"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Fatalf("ParseByteSize(%q) must fail", bad)
		}
	}
}

// When an upload would exceed the quota the relay collects once before refusing.
func TestVaultQuotaPressureCollects(t *testing.T) {
	f := newVaultFixture(t)
	f.s.vaultGCEvery = 0
	junk := f.putBlob(t, "", bytes.Repeat([]byte("j"), 80))
	f.age(t, junk)
	f.s.SetVaultQuota(100)
	data := bytes.Repeat([]byte("n"), 60)
	if code, body := f.raw(t, "PUT", f.vpath("/blob/"+blobID(data)), "", data); code != 200 {
		t.Fatalf("upload after reclaiming unreferenced junk: %d %s", code, body)
	}
	if f.stored(junk) {
		t.Fatal("the unreferenced, expired blob should have been collected")
	}
	if u := f.usage(t); u.Bytes != 60 || u.Blobs != 1 {
		t.Fatalf("usage after reclaim: %+v", u)
	}
}

func TestVaultGCRetention(t *testing.T) {
	f := newVaultFixture(t)
	// main: versions 1..4 (v1 falls out of the retained three); a dev lane at v1.
	var roots, refss []string
	var contents [][]string
	for v := 1; v <= 4; v++ {
		root, refs, ids := f.putVersion(t, "dev-A", fmt.Sprint("v", v), fmt.Sprint("only-in-v", v), "shared")
		if code, body := f.setHead(t, "dev-A", "main", int64(v-1), root, refs); code != 200 {
			t.Fatalf("v%d: %d %v", v, code, body)
		}
		roots, refss, contents = append(roots, root), append(refss, refs), append(contents, ids)
	}
	devRoot, devRefs, devIDs := f.putVersion(t, "dev-B", "dev", "dev-only")
	if code, body := f.setHead(t, "dev-B", "dev-dev-B", 0, devRoot, devRefs); code != 200 {
		t.Fatalf("dev lane: %d %v", code, body)
	}
	junkOld := f.putBlob(t, "", []byte("junk, old"))
	junkNew := f.putBlob(t, "", []byte("junk, just uploaded (in flight)"))
	confirmed := f.putBlob(t, "", []byte("old, but has() just confirmed it"))

	// Age everything, then give back the grace to what is "recent".
	for _, id := range append(append(append([]string{}, roots...), refss...), junkOld, confirmed, devRoot, devRefs, devIDs[0]) {
		f.age(t, id)
	}
	for _, ids := range contents {
		for _, id := range ids {
			f.age(t, id)
		}
	}
	if code, body := f.do(t, "POST", f.vpath("/has"), "", "", map[string]any{"ids": []string{confirmed}}); code != 200 {
		t.Fatalf("has: %d %v", code, body)
	}
	// Leave a stale upload temp behind as well.
	tmpDir := f.s.vaultBoxDir(f.box) + "/tmp"
	_ = os.MkdirAll(tmpDir, 0o755)
	stale := tmpDir + "/put-stale"
	_ = os.WriteFile(stale, []byte("x"), 0o644)
	old := time.Now().Add(-vaultGrace - time.Hour)
	_ = os.Chtimes(stale, old, old)

	before := f.usage(t)
	removed, err := f.s.vaultGC(f.box)
	if err != nil {
		t.Fatal(err)
	}
	// v1's own blobs are gone: its root, its refs and the content only it used.
	for _, id := range []string{roots[0], refss[0], contents[0][0], junkOld} {
		if f.stored(id) {
			t.Fatalf("blob %s should have been collected", id[:8])
		}
	}
	// v2..v4 (retained), the shared blob, the dev lane, the fresh and the confirmed blob stay.
	keep := []string{contents[0][1], junkNew, confirmed, devRoot, devRefs, devIDs[0]}
	for v := 1; v < 4; v++ {
		keep = append(keep, roots[v], refss[v], contents[v][0])
	}
	for _, id := range keep {
		if !f.stored(id) {
			t.Fatalf("blob %s must survive collection", id[:8])
		}
	}
	if removed != 4 {
		t.Fatalf("want 4 blobs removed, got %d", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a stale upload temp file should have been removed")
	}
	after := f.usage(t)
	if after.Blobs != before.Blobs-4 || after.Bytes >= before.Bytes {
		t.Fatalf("usage must shrink by what was collected: before %+v after %+v", before, after)
	}
	// Collecting again is a no-op.
	if n, err := f.s.vaultGC(f.box); err != nil || n != 0 {
		t.Fatalf("second collection: %d %v", n, err)
	}
}

// A retained refs blob that went missing aborts the collection: nothing is deleted.
func TestVaultGCAbortsOnUnreadableRefs(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, _ := f.putVersion(t, "", "v1", "a")
	if code, body := f.setHead(t, "", "main", 0, root, refs); code != 200 {
		t.Fatalf("main: %d %v", code, body)
	}
	junk := f.putBlob(t, "", []byte("junk"))
	f.age(t, junk)
	if err := os.Remove(f.s.vaultBlobPath(f.box, refs)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.vaultGC(f.box); err == nil {
		t.Fatal("collection must fail when a retained refs blob is unreadable")
	}
	if !f.stored(junk) {
		t.Fatal("an aborted collection must not delete anything")
	}
}

// Head updates schedule a collection on their own (debounced, asynchronous).
func TestVaultGCRunsAfterHeadUpdate(t *testing.T) {
	f := newVaultFixture(t)
	f.s.vaultGCDelay, f.s.vaultGCEvery = 10*time.Millisecond, 0
	junk := f.putBlob(t, "", []byte("junk"))
	f.age(t, junk)
	root, refs, _ := f.putVersion(t, "", "v1", "a")
	if code, body := f.setHead(t, "", "main", 0, root, refs); code != 200 {
		t.Fatalf("main: %d %v", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.stored(junk) {
		if time.Now().After(deadline) {
			t.Fatal("the head update should have triggered a collection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !f.stored(root) || !f.stored(refs) {
		t.Fatal("the new version must survive the triggered collection")
	}
}

// Usage and heads survive a relay restart (usage is rebuilt by scanning, heads are files).
func TestVaultSurvivesRestart(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, _ := f.putVersion(t, "dev-A", "v1", "a", "b")
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("main: %d %v", code, body)
	}
	before := f.usage(t)

	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	s2.vaultGCDelay = -1
	srv2 := httptest.NewServer(s2.Handler())
	t.Cleanup(srv2.Close)
	f2 := &deviceFixture{s: s2, srv: srv2, id: f.id, box: f.box}
	if after := f2.usage(t); after.Bytes != before.Bytes || after.Blobs != before.Blobs || after.Blobs != 4 {
		t.Fatalf("usage after restart: before %+v after %+v", before, after)
	}
	code, raw := f2.raw(t, "GET", f2.vpath("/head/main"), "dev-A", nil)
	var h a2a.VaultHead
	_ = json.Unmarshal(raw, &h)
	if code != 200 || h.Version != 1 || h.Root != root || h.Refs != refs || h.Device != "dev-A" {
		t.Fatalf("head after restart: %d %s", code, raw)
	}
	// And the CAS continues from the persisted version.
	root2, refs2, _ := f2.putVersion(t, "dev-A", "v2", "c")
	if code, body := f2.setHead(t, "dev-A", "main", 1, root2, refs2); code != 200 {
		t.Fatalf("advance after restart: %d %v", code, body)
	}
}

// The lane alphabet of a2a.ValidVaultLane must stay the device-id alphabet of ValidDeviceID.
func TestVaultLaneMatchesDeviceIDRule(t *testing.T) {
	for _, d := range []string{"dev-A", "a", "Z9._~=-", strings.Repeat("x", 128), strings.Repeat("x", 129), "", "a/b", "a b", "a%b", "é"} {
		if got, want := a2a.ValidVaultLane(a2a.VaultDevLane(d)), ValidDeviceID(d); got != want {
			t.Fatalf("device id %q: ValidVaultLane=%v but ValidDeviceID=%v", d, got, want)
		}
	}
	if !a2a.ValidVaultLane("main") || a2a.ValidVaultLane("Main") || a2a.ValidVaultLane("main2") {
		t.Fatal("main lane rule")
	}
}

func TestVaultPurge(t *testing.T) {
	f := newVaultFixture(t)
	root, refs, ids := f.putVersion(t, "dev-A", "v1", "a", "b")
	if code, body := f.setHead(t, "dev-A", "main", 0, root, refs); code != 200 {
		t.Fatalf("main: %d %v", code, body)
	}
	if code, body := f.setHead(t, "dev-B", "dev-dev-B", 0, root, refs); code != 200 {
		t.Fatalf("dev lane: %d %v", code, body)
	}
	// A non-active device may not wipe the vault; nothing is touched.
	code, body := f.do(t, "DELETE", f.vpath(""), "", "dev-B", nil)
	assertKicked(t, code, body, "dev-A")
	if u := f.usage(t); u.Blobs != 4 {
		t.Fatalf("a refused purge must not remove anything: %+v", u)
	}
	code, body = f.do(t, "DELETE", f.vpath(""), "", "dev-A", nil)
	if code != 200 || body["blobs"] != float64(4) {
		t.Fatalf("purge: %d %v", code, body)
	}
	if u := f.usage(t); u.Bytes != 0 || u.Blobs != 0 {
		t.Fatalf("usage after purge: %+v", u)
	}
	if code, raw := f.raw(t, "GET", f.vpath("/heads"), "dev-A", nil); code != 200 || !strings.Contains(string(raw), `"heads":[]`) {
		t.Fatalf("heads after purge: %d %s", code, raw)
	}
	if code, _ := f.raw(t, "GET", f.vpath("/blob/"+ids[0]), "dev-A", nil); code != 404 {
		t.Fatalf("old blob after purge: want 404, got %d", code)
	}
	if code, _ := f.raw(t, "GET", f.vpath("/head/main"), "dev-A", nil); code != 404 {
		t.Fatalf("old head after purge: want 404, got %d", code)
	}
	// Idempotent: a second purge (no vault left) is still 200.
	if code, body = f.do(t, "DELETE", f.vpath(""), "", "dev-A", nil); code != 200 || body["blobs"] != float64(0) {
		t.Fatalf("second purge: %d %v", code, body)
	}
	// The vault starts over cleanly: main is created from version 0 again, usage counts afresh.
	root2, refs2, _ := f.putVersion(t, "dev-A", "v2", "c")
	if code, body = f.setHead(t, "dev-A", "main", 0, root2, refs2); code != 200 {
		t.Fatalf("recreate main after purge: %d %v", code, body)
	}
	if u := f.usage(t); u.Blobs != 3 {
		t.Fatalf("usage after starting over: %+v", u)
	}
	// And the counters stay right across a restart after a purge.
	s2, err := New(f.s.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	vb := s2.vaultBoxFor(f.box)
	vb.mu.Lock()
	err = s2.vaultLoadLocked(f.box, vb)
	blobs, heads := vb.blobs, len(vb.heads)
	vb.mu.Unlock()
	if err != nil || blobs != 3 || heads != 1 {
		t.Fatalf("rescan after purge + restart: blobs=%d heads=%d err=%v", blobs, heads, err)
	}
}
