package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

func vaultTestID(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// vaultTwoDevices returns two peers holding the same identity (dev-A, dev-B) on a real
// local relay, plus that relay.
func vaultTwoDevices(t *testing.T) (a, b *Peer, rs interface{ SetVaultQuota(int64) }) {
	t.Helper()
	srv, url := startRelayServer(t)
	a = newIdlePeer(t, url)
	a.DeviceID, a.DeviceName = "dev-A", "Desktop"
	b, err := Init(filepath.Join(t.TempDir(), "home-b"), url)
	if err != nil {
		t.Fatal(err)
	}
	b.Logf = a.Logf
	b.SetIdentity(a.Identity())
	b.DeviceID, b.DeviceName = "dev-B", "Laptop"
	return a, b, srv
}

// vaultPutAll uploads contents and returns their ids.
func vaultPutAll(t *testing.T, n *Peer, contents ...string) []string {
	t.Helper()
	var ids []string
	for _, c := range contents {
		id := vaultTestID([]byte(c))
		if _, err := n.VaultPut(context.Background(), id, []byte(c)); err != nil {
			t.Fatalf("VaultPut %q: %v", c, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// vaultPutVersion uploads contents + a manifest + a refs blob; returns (root, refs).
func vaultPutVersion(t *testing.T, n *Peer, tag string, contents ...string) (string, string) {
	t.Helper()
	ids := vaultPutAll(t, n, contents...)
	root := vaultPutAll(t, n, "manifest "+tag)[0]
	refs := vaultTestID(a2a.EncodeVaultRefs(ids))
	if _, err := n.VaultPut(context.Background(), refs, a2a.EncodeVaultRefs(ids)); err != nil {
		t.Fatal(err)
	}
	return root, refs
}

func TestVaultRoundTripThroughPeer(t *testing.T) {
	ctx := context.Background()
	a, b, _ := vaultTwoDevices(t)

	// Empty vault: no lane, no heads, zero usage.
	if h, err := a.VaultHead(ctx, VaultMainLane); err != nil || h != nil {
		t.Fatalf("head of an empty vault: %+v %v", h, err)
	}
	if hs, err := a.VaultHeads(ctx); err != nil || len(hs) != 0 {
		t.Fatalf("heads of an empty vault: %+v %v", hs, err)
	}
	if _, err := a.VaultGet(ctx, vaultTestID([]byte("nothing"))); !errors.Is(err, ErrVaultNotFound) {
		t.Fatalf("missing blob: want ErrVaultNotFound, got %v", err)
	}

	// Put is idempotent; has reports only what is missing.
	data := []byte("ciphertext")
	id := vaultTestID(data)
	if existed, err := a.VaultPut(ctx, id, data); err != nil || existed {
		t.Fatalf("first put: existed=%v err=%v", existed, err)
	}
	if existed, err := b.VaultPut(ctx, id, data); err != nil || !existed {
		t.Fatalf("second put (other device, same identity): existed=%v err=%v", existed, err)
	}
	absent := vaultTestID([]byte("absent"))
	if missing, err := a.VaultHas(ctx, []string{id, absent}); err != nil || len(missing) != 1 || missing[0] != absent {
		t.Fatalf("has: %v %v", missing, err)
	}
	if got, err := b.VaultGet(ctx, id); err != nil || string(got) != string(data) {
		t.Fatalf("get: %q %v", got, err)
	}

	// main: A creates it (and so becomes the active device), advances it with CAS.
	root1, refs1 := vaultPutVersion(t, a, "v1", "x", "y")
	h, err := a.VaultSetHead(ctx, VaultMainLane, 0, root1, refs1)
	if err != nil || h.Version != 1 || h.Root != root1 || h.Device != "dev-A" {
		t.Fatalf("create main: %+v %v", h, err)
	}
	root2, refs2 := vaultPutVersion(t, a, "v2", "z")
	_, err = a.VaultSetHead(ctx, VaultMainLane, 0, root2, refs2)
	var conflict *VaultConflictError
	if !errors.Is(err, ErrVaultConflict) || !errors.As(err, &conflict) || conflict.Current != 1 || conflict.Root != root1 || conflict.Lane != VaultMainLane {
		t.Fatalf("stale CAS: want *VaultConflictError at 1, got %v", err)
	}
	if errors.Is(err, ErrNetwork) {
		t.Fatal("a CAS conflict is a relay verdict, not a network failure")
	}
	if h, err = a.VaultSetHead(ctx, VaultMainLane, 1, root2, refs2); err != nil || h.Version != 2 {
		t.Fatalf("advance main: %+v %v", h, err)
	}

	// Missing blobs are refused with the ids.
	ghost := vaultTestID([]byte("never uploaded"))
	badRefs := vaultTestID(a2a.EncodeVaultRefs([]string{ghost}))
	if _, err := a.VaultPut(ctx, badRefs, a2a.EncodeVaultRefs([]string{ghost})); err != nil {
		t.Fatal(err)
	}
	_, err = a.VaultSetHead(ctx, VaultMainLane, 2, root2, badRefs)
	var miss *VaultMissingError
	if !errors.Is(err, ErrVaultMissing) || !errors.As(err, &miss) || miss.Count != 1 || miss.IDs[0] != ghost {
		t.Fatalf("missing blobs: want *VaultMissingError, got %v", err)
	}

	// B is not active: main answers kicked; its own dev lane is fine; A's dev lane is 403.
	_, err = b.VaultSetHead(ctx, VaultMainLane, 2, root2, refs2)
	var k *ErrKicked
	if !errors.As(err, &k) || k.ActiveDevice != "dev-A" {
		t.Fatalf("non-active device on main: want *ErrKicked, got %v", err)
	}
	if b.VaultDevLane() != "dev-dev-B" {
		t.Fatalf("VaultDevLane = %q", b.VaultDevLane())
	}
	if h, err = b.VaultSetHead(ctx, b.VaultDevLane(), 0, root2, refs2); err != nil || h.Version != 1 || h.Device != "dev-B" {
		t.Fatalf("B's own lane: %+v %v", h, err)
	}
	_, err = b.VaultSetHead(ctx, a.VaultDevLane(), 0, root2, refs2)
	var re *a2a.RelayError
	if !errors.As(err, &re) || re.StatusCode != http.StatusForbidden {
		t.Fatalf("B on A's lane: want 403, got %v", err)
	}

	hs, err := a.VaultHeads(ctx)
	if err != nil || len(hs) != 2 || hs[0].Lane != "dev-dev-B" || hs[1].Lane != VaultMainLane || hs[1].Version != 2 {
		t.Fatalf("heads: %+v %v", hs, err)
	}
	// The active device drops B's lane once merged; deleting it again is not an error.
	if err := a.VaultDeleteHead(ctx, b.VaultDevLane(), 1); err != nil {
		t.Fatalf("drop merged dev lane: %v", err)
	}
	if err := a.VaultDeleteHead(ctx, b.VaultDevLane(), 1); err != nil {
		t.Fatalf("drop again: %v", err)
	}

	u, err := a.VaultUsage(ctx)
	if err != nil || u.Blobs == 0 || u.Bytes == 0 || u.Quota <= 0 {
		t.Fatalf("usage: %+v %v", u, err)
	}

	// Purge: B (not active) is kicked; A wipes everything; purging again succeeds.
	if err := b.VaultPurge(ctx); !IsKicked(err) {
		t.Fatalf("purge from a non-active device: want kicked, got %v", err)
	}
	if err := a.VaultPurge(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if u, err = a.VaultUsage(ctx); err != nil || u.Bytes != 0 || u.Blobs != 0 {
		t.Fatalf("usage after purge: %+v %v", u, err)
	}
	if hs, err = a.VaultHeads(ctx); err != nil || len(hs) != 0 {
		t.Fatalf("heads after purge: %+v %v", hs, err)
	}
	if _, err := a.VaultGet(ctx, id); !errors.Is(err, ErrVaultNotFound) {
		t.Fatalf("blob after purge: want ErrVaultNotFound, got %v", err)
	}
	if err := a.VaultPurge(ctx); err != nil {
		t.Fatalf("second purge: %v", err)
	}
}

func TestVaultQuotaThroughPeer(t *testing.T) {
	ctx := context.Background()
	a, _, rs := vaultTwoDevices(t)
	rs.SetVaultQuota(10)
	vaultPutAll(t, a, "12345678")
	_, err := a.VaultPut(ctx, vaultTestID([]byte("abcdefgh")), []byte("abcdefgh"))
	if !errors.Is(err, ErrVaultQuota) {
		t.Fatalf("over quota: want ErrVaultQuota, got %v", err)
	}
	var re *a2a.RelayError
	if !errors.As(err, &re) || re.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("the relay status must stay reachable: %v", err)
	}
}

// A relay without the vault (an older release) is reported as such, not as "no backup yet".
func TestVaultUnsupportedRelay(t *testing.T) {
	ctx := context.Background()
	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	n := newIdlePeer(t, old.URL)
	if _, err := n.VaultHead(ctx, VaultMainLane); !errors.Is(err, ErrVaultUnsupported) {
		t.Fatalf("head on an old relay: want ErrVaultUnsupported, got %v", err)
	}
	if _, err := n.VaultHeads(ctx); !errors.Is(err, ErrVaultUnsupported) {
		t.Fatalf("heads on an old relay: want ErrVaultUnsupported, got %v", err)
	}
	if _, err := n.VaultPut(ctx, vaultTestID([]byte("x")), []byte("x")); !errors.Is(err, ErrVaultUnsupported) {
		t.Fatalf("put on an old relay: want ErrVaultUnsupported, got %v", err)
	}
}

func TestVaultWithoutIdentity(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "home"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.VaultUsage(context.Background()); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("want ErrNoIdentity, got %v", err)
	}
	if err := n.VaultPurge(context.Background()); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("want ErrNoIdentity, got %v", err)
	}
}
