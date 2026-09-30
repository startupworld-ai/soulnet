package relay

import (
	"context"
	"errors"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

const testAvatarSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 128 128"><circle cx="64" cy="64" r="60" fill="#2f8a78"/></svg>`

// The owner uploads an avatar, anyone reads it back (with 304 revalidation), a scripted SVG and an
// upload signed by someone else are refused, and deleting clears it.
func TestAvatarPutFetchDelete(t *testing.T) {
	f := newDeviceFixture(t)
	ctx := context.Background()
	pc := a2a.NewProxyClient(f.srv.URL, f.id)

	if _, _, _, _, err := a2a.FetchAvatar(ctx, f.srv.URL, f.box, ""); !errors.Is(err, a2a.ErrNoAvatar) {
		t.Fatalf("no avatar yet: want ErrNoAvatar, got %v", err)
	}
	if err := pc.PutAvatar(ctx, []byte(`<svg onload="alert(1)"></svg>`), "image/svg+xml"); err == nil {
		t.Fatal("scripted svg must be refused")
	}
	if err := pc.PutAvatar(ctx, []byte(testAvatarSVG), "image/svg+xml"); err != nil {
		t.Fatalf("PutAvatar: %v", err)
	}
	data, ct, etag, _, err := a2a.FetchAvatar(ctx, f.srv.URL, f.box, "")
	if err != nil || string(data) != testAvatarSVG || ct != "image/svg+xml" || etag == "" {
		t.Fatalf("fetch: err=%v ct=%q etag=%q data=%q", err, ct, etag, data)
	}
	if _, _, _, notModified, err := a2a.FetchAvatar(ctx, f.srv.URL, f.box, etag); err != nil || !notModified {
		t.Fatalf("same etag should be 304: %v %v", notModified, err)
	}

	// Someone else cannot overwrite this mailbox's avatar: their signature proves a different box.
	other, err := a2a.NewIdentity(t.TempDir(), "other", []string{f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	intruder := a2a.NewProxyClient(f.srv.URL, other)
	intruder.Base = f.srv.URL
	if err := intruder.PutAvatar(ctx, []byte(testAvatarSVG), "image/svg+xml"); err != nil {
		t.Fatalf("intruder uploads to its own box: %v", err)
	}
	if data, _, _, _, _ := a2a.FetchAvatar(ctx, f.srv.URL, f.box, ""); string(data) != testAvatarSVG {
		t.Fatal("the owner's avatar must be untouched")
	}

	if err := pc.DeleteAvatar(ctx); err != nil {
		t.Fatalf("DeleteAvatar: %v", err)
	}
	if _, _, _, _, err := a2a.FetchAvatar(ctx, f.srv.URL, f.box, ""); !errors.Is(err, a2a.ErrNoAvatar) {
		t.Fatalf("after delete: want ErrNoAvatar, got %v", err)
	}
}
