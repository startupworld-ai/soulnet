package a2a

import (
	"strings"
	"testing"
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
