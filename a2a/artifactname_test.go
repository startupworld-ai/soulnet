package a2a

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidArtifactName(t *testing.T) {
	ok := []string{"report.pdf", "截图 2026-09-27.png", "a.b.c.txt", "v1.2.3.tar.gz", "(1) notes.md", strings.Repeat("a", MaxArtifactNameLen)}
	for _, n := range ok {
		if !ValidArtifactName(n) {
			t.Errorf("%q rejected", n)
		}
	}
	bad := []string{"", ".", "..", "../x", "x/..", "a/b", `a\b`, "c:x", "a..b", "tab\tname", "nul\x00", strings.Repeat("a", MaxArtifactNameLen+1)}
	for _, n := range bad {
		if ValidArtifactName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

// TestGroupInlineFitsRelayBody pins the arithmetic behind GroupMaxArtifactBytes: an
// inline group attachment is base64'd three times on its way into POST /group/mail
// (message JSON → cipher blob "c" → Envelope.cipher), and must stay under the relay's
// 1 MiB body limit with room for the envelope.
func TestGroupInlineFitsRelayBody(t *testing.T) {
	b64 := func(n int) int { return (n + 2) / 3 * 4 }
	wire := b64(b64(b64(GroupMaxArtifactBytes)+1024) + 64)
	if wire > (1<<20)-16*1024 {
		t.Fatalf("an inline group attachment at the limit is %d bytes on the wire", wire)
	}
	if GroupChunkRawBytes > GroupMaxArtifactBytes {
		t.Fatalf("a group chunk (%d) is larger than the inline limit (%d)", GroupChunkRawBytes, GroupMaxArtifactBytes)
	}
	if !GroupShouldChunk(GroupMaxArtifactBytes+1) || GroupShouldChunk(GroupMaxArtifactBytes) {
		t.Fatal("GroupShouldChunk threshold")
	}
	if got := len(SplitChunksOf(make([]byte, 2*GroupChunkRawBytes+1), GroupChunkRawBytes)); got != 3 {
		t.Fatalf("SplitChunksOf: %d parts", got)
	}
}

// TestPairwiseInlineFitsRelayBody pins the arithmetic behind MaxArtifactBytes with a real
// sealed envelope: an inline pairwise attachment is base64'd twice on its way into
// POST /mail (message JSON → Envelope.cipher). At the limit, with a card attached and a
// 16 KiB text, the envelope must stay under the relay's 1 MiB body; the old 700 KiB cap
// must not (that is the bug this constant fixes: 600-700 KiB files were refused with 400).
func TestPairwiseInlineFitsRelayBody(t *testing.T) {
	a, b := newTestIdentity(t, "a"), newTestIdentity(t, "b")
	ac, _ := a.Card()
	bc, _ := b.Card()
	wire := func(raw int) int {
		msg := &Message{ID: "a-0000000000000000001-000000000001", From: a.Fingerprint(), To: b.Fingerprint(),
			TS: time.Now(), Type: TypeText, Body: strings.Repeat("x", 16*1024), Card: ac,
			ArtifactName: strings.Repeat("名", 60) + ".bin",
			Artifact:     base64.StdEncoding.EncodeToString(make([]byte, raw))}
		env, err := SealEnvelope(a, bc, msg)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(env)
		return len(out)
	}
	if n := wire(MaxArtifactBytes); n >= 1<<20 {
		t.Fatalf("an inline attachment at MaxArtifactBytes is %d bytes on the wire (relay reads 1 MiB)", n)
	}
	if n := wire(700 * 1024); n < 1<<20 {
		t.Fatalf("sanity: 700 KiB inline should overflow the relay body, got %d", n)
	}
	if ChunkRawBytes > MaxArtifactBytes {
		t.Fatalf("a chunk (%d) must not be larger than the inline limit (%d)", ChunkRawBytes, MaxArtifactBytes)
	}
	if !ShouldChunk(MaxArtifactBytes+1) || ShouldChunk(MaxArtifactBytes) {
		t.Fatal("ShouldChunk threshold")
	}
}
