package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// fileEquals reports whether path holds exactly want.
func fileEquals(path string, want []byte) bool {
	got, err := os.ReadFile(path)
	return err == nil && bytes.Equal(got, want)
}

// TestGroupAttachments drives group attachments over a real relay (which enforces the
// 1 MiB body limit on POST /group/mail): an inline image with a caption, an inline file
// right at GroupMaxArtifactBytes with no text, a chunked file, and a post from a member
// to a co-member who is NOT their friend. Receivers keep the bytes on disk (never in the
// archive), the sender keeps its own copy, and chunk frames neither show up in the
// conversation nor count as unread.
func TestGroupAttachments(t *testing.T) {
	relayURL := startRelay(t)
	alice := newTestNode(t, relayURL, "alice")
	bob := newTestNode(t, relayURL, "bob")
	carol := newTestNode(t, relayURL, "carol")
	befriend(t, alice, bob)
	befriend(t, alice, carol)

	ctx := context.Background()
	view, err := alice.GroupCreate(ctx, "files", []string{bob.Fingerprint(), carol.Fingerprint()}, nil)
	if err != nil {
		t.Fatalf("GroupCreate: %v", err)
	}
	gid := view.GID
	bob.await(t, "bob joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	carol.await(t, "carol joins", func(e Event) bool { return e.Kind == EventGroupUpdated && e.GID == gid })
	dir := GroupArtifactPeer(gid)

	// 1) Inline image with a caption.
	img := randomBytes(t, 300*1024)
	res, err := alice.GroupSend(ctx, gid, "look at this", GroupSendOptions{Attachment: &Attachment{Name: "shot.png", Raw: img}})
	if err != nil || res.Status != "sent" || res.Chunks != 0 {
		t.Fatalf("inline GroupSend: %+v %v", res, err)
	}
	if !fileEquals(alice.ArtifactPath(dir, res.ID, "shot.png"), img) {
		t.Fatal("sender did not keep its own copy of the inline attachment")
	}
	ev := bob.await(t, "bob gets the image", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.ArtifactName == "shot.png"
	})
	if ev.Message.Body != "look at this" || ev.Message.Artifact != "" || ev.Message.ArtifactSize != int64(len(img)) {
		t.Fatalf("bob's event message: body=%q artifact=%d bytes size=%d", ev.Message.Body, len(ev.Message.Artifact), ev.Message.ArtifactSize)
	}
	if ev.ArtifactPath == "" || !fileEquals(ev.ArtifactPath, img) {
		t.Fatalf("bob's inline attachment on disk: path=%q", ev.ArtifactPath)
	}
	if p, err := bob.ArtifactFile(dir, ev.Message.ID, "shot.png"); err != nil || p != ev.ArtifactPath {
		t.Fatalf("ArtifactFile under GroupArtifactPeer: %q %v", p, err)
	}

	// 2) Inline right at the limit, no text: must still fit the relay's body limit.
	edge := randomBytes(t, a2a.GroupMaxArtifactBytes)
	res, err = alice.GroupSend(ctx, gid, "", GroupSendOptions{Attachment: &Attachment{Name: "edge.bin", Raw: edge}})
	if err != nil || res.Status != "sent" || res.Chunks != 0 {
		t.Fatalf("edge GroupSend: %+v %v", res, err)
	}
	ev = bob.await(t, "bob gets the edge file", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.ArtifactName == "edge.bin"
	})
	if ev.Message.Body != "" || !fileEquals(ev.ArtifactPath, edge) {
		t.Fatalf("edge file: body=%q path=%q", ev.Message.Body, ev.ArtifactPath)
	}

	// 3) Chunked: announcement first, parts follow as artifact_chunk fan-outs.
	big := randomBytes(t, 2*a2a.GroupChunkRawBytes+12345)
	res, err = alice.GroupSend(ctx, gid, "the report", GroupSendOptions{Attachment: &Attachment{Name: "report.pdf", Raw: big}})
	if err != nil || res.Status != "sent" || res.Chunks != 3 {
		t.Fatalf("chunked GroupSend: %+v %v", res, err)
	}
	ev = bob.await(t, "bob gets the announcement", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.ArtifactName == "report.pdf"
	})
	aid := ev.Message.ArtifactID
	if aid == "" || ev.Message.ChunkTotal != 3 || ev.Message.ArtifactSHA != a2a.SHA256Hex(big) {
		t.Fatalf("announcement metadata: %+v", ev.Message)
	}
	if !fileEquals(alice.ArtifactPath(dir, aid, "report.pdf"), big) {
		t.Fatal("sender did not keep its own copy of the chunked attachment")
	}
	for _, n := range []*testNode{bob, carol} {
		n := n
		waitUntil(t, "chunked file reassembled", func() bool { return fileEquals(n.ArtifactPath(dir, aid, "report.pdf"), big) })
		if _, err := os.Stat(n.IncomingDir(dir, aid)); !os.IsNotExist(err) {
			t.Fatalf("staging dir left behind: %v", err)
		}
	}
	entries := carol.GroupConversation(gid, 0, 0)
	if len(entries) != 3 {
		t.Fatalf("carol archive: want 3 visible entries (no chunk frames), got %d", len(entries))
	}
	for _, e := range entries {
		if e.Type != a2a.TypeText || e.Artifact != "" {
			t.Fatalf("archived entry: type=%q artifact=%d bytes", e.Type, len(e.Artifact))
		}
	}
	if s := carol.GroupList(); len(s) != 1 || s[0].Unread != 3 || s[0].LastArtifact != "report.pdf" || s[0].LastBody != "the report" {
		t.Fatalf("carol summary: %+v", s)
	}

	// 4) bob → carol are not friends: group attachments must not need it.
	doc := []byte("minutes of the meeting")
	if _, err := bob.GroupSend(ctx, gid, "", GroupSendOptions{Attachment: &Attachment{Name: "minutes.txt", Raw: doc}}); err != nil {
		t.Fatalf("bob GroupSend: %v", err)
	}
	ev = carol.await(t, "carol gets bob's file", func(e Event) bool {
		return e.Kind == EventGroupMessage && e.GID == gid && e.Message.ArtifactName == "minutes.txt"
	})
	if ev.Peer != bob.Fingerprint() || !fileEquals(ev.ArtifactPath, doc) {
		t.Fatalf("carol got %+v", ev)
	}
}

// TestGroupAttachmentOptions covers the send-side rules without a network.
func TestGroupAttachmentOptions(t *testing.T) {
	if att, err := groupAttachment(GroupSendOptions{}); att != nil || err != nil {
		t.Fatalf("no attachment: %+v %v", att, err)
	}
	cases := []struct {
		name string
		att  *Attachment
		want error
	}{
		{"traversal name", &Attachment{Name: "../../evil.sh", Raw: []byte("x")}, ErrBadFile},
		{"separator", &Attachment{Name: `a\b.txt`, Raw: []byte("x")}, ErrBadFile},
		{"empty bytes", &Attachment{Name: "a.txt"}, ErrBadFile},
		{"too big", &Attachment{Name: "a.bin", Raw: make([]byte, MaxGroupFileBytes+1)}, ErrArtifactSize},
	}
	for _, c := range cases {
		if _, err := groupAttachment(GroupSendOptions{Attachment: c.att}); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	p := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(p, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	att, err := groupAttachment(GroupSendOptions{File: p})
	if err != nil || att.Name != "notes.md" || string(att.Raw) != "# hi" {
		t.Fatalf("File option: %+v %v", att, err)
	}
}

// TestAcceptGroupAttachment covers the receive-side rules: unsafe names and bad bytes
// drop the attachment (keeping the text), an inline file lands on disk with its real
// size, a name without bytes or transfer is cleared, and parts that outran their
// announcement are assembled when it arrives.
func TestAcceptGroupAttachment(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "home"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	gid := "0123456789abcdef0123456789abcdef"
	dir := GroupArtifactPeer(gid)
	b64 := base64.StdEncoding.EncodeToString

	m := &a2a.Message{ID: "m1", Body: "hi", ArtifactName: "../x.png", Artifact: b64([]byte("img"))}
	if p := n.acceptGroupAttachment(gid, m); p != "" || m.ArtifactName != "" || m.Artifact != "" || m.Body != "hi" {
		t.Fatalf("unsafe name: path=%q msg=%+v", p, m)
	}
	m = &a2a.Message{ID: "m2", ArtifactName: "a.png", Artifact: "%%%not base64"}
	if p := n.acceptGroupAttachment(gid, m); p != "" || m.ArtifactName != "" {
		t.Fatalf("bad base64: path=%q msg=%+v", p, m)
	}
	m = &a2a.Message{ID: "m3", ArtifactName: "a.png", Artifact: b64([]byte("img")), ArtifactSize: 999999}
	p := n.acceptGroupAttachment(gid, m)
	if p != n.ArtifactPath(dir, "m3", "a.png") || !fileEquals(p, []byte("img")) || m.Artifact != "" || m.ArtifactSize != 3 {
		t.Fatalf("inline: path=%q msg=%+v", p, m)
	}
	m = &a2a.Message{ID: "m4", ArtifactName: "ghost.pdf"}
	if p := n.acceptGroupAttachment(gid, m); p != "" || m.ArtifactName != "" {
		t.Fatalf("name without bytes: path=%q msg=%+v", p, m)
	}

	// Parts first, announcement last.
	raw := []byte(strings.Repeat("0123456789", 10))
	parts := a2a.SplitChunksOf(raw, 40)
	aid := a2a.NewArtifactID()
	for i, c := range parts {
		chunk := &a2a.Message{ID: "c" + string(rune('0'+i)), Type: a2a.TypeArtifactChunk, ArtifactID: aid, ArtifactName: "d.bin",
			ChunkIndex: i, ChunkTotal: len(parts), ArtifactSHA: a2a.SHA256Hex(raw), Artifact: b64(c)}
		if i == len(parts)-1 {
			break // hold back the last part: the announcement must not assemble early
		}
		if err := n.handleGroupArtifactChunk(gid, "fp", chunk); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	ann := &a2a.Message{ID: "m5", ArtifactName: "d.bin", ArtifactID: aid, ChunkTotal: len(parts), ArtifactSHA: a2a.SHA256Hex(raw)}
	if p := n.acceptGroupAttachment(gid, ann); p != "" || ann.ArtifactID != aid {
		t.Fatalf("incomplete transfer: path=%q msg=%+v", p, ann)
	}
	last := len(parts) - 1
	lastChunk := &a2a.Message{ID: "cz", Type: a2a.TypeArtifactChunk, ArtifactID: aid, ArtifactName: "d.bin",
		ChunkIndex: last, ChunkTotal: len(parts), ArtifactSHA: a2a.SHA256Hex(raw), Artifact: b64(parts[last])}
	if _, err := n.stageArtifactChunk(dir, lastChunk); err != nil {
		t.Fatalf("stage last: %v", err)
	}
	ann = &a2a.Message{ID: "m6", ArtifactName: "d.bin", ArtifactID: aid, ChunkTotal: len(parts), ArtifactSHA: a2a.SHA256Hex(raw)}
	if p := n.acceptGroupAttachment(gid, ann); p == "" || !fileEquals(p, raw) {
		t.Fatalf("parts outran the announcement: path=%q", p)
	}
	// A redelivered part after completion is ignored (no stray staging dir).
	if err := n.handleGroupArtifactChunk(gid, "fp", lastChunk); err != nil {
		t.Fatalf("redelivered part: %v", err)
	}
	if _, err := os.Stat(n.IncomingDir(dir, aid)); !os.IsNotExist(err) {
		t.Fatalf("redelivered part recreated the staging dir: %v", err)
	}
}

// TestPersistArtifactBytesRefusesUnsafeNames: the attachment name comes from the sender
// (friend or group co-member) and must never steer the write out of the artifacts dir.
func TestPersistArtifactBytesRefusesUnsafeNames(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "home"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, name := range []string{"../../../../escape.txt", `..\..\escape.txt`, "a/b.txt", "", "..", "c:evil"} {
		if p := n.PersistArtifactBytes("peer", "key", name, []byte("x")); p != "" {
			t.Errorf("name %q was written to %s", name, p)
		}
	}
	if p := n.PersistArtifactBytes("peer", "key", "ok file (1).txt", []byte("x")); p == "" {
		t.Error("a normal name was refused")
	}
}
