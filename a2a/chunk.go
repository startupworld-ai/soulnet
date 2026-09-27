package a2a

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ——— Deliverables are always zipped ———
//
// Delivery no longer "sends just one file" (alters often produce several files but fill in only one path → files get missed, and the UI cannot show them well).
// Instead: zip [all files] in the delivery directory into one archive and send that; a single file is zipped too, so the UI always shows one attachment.
//
// ZipArtifactDir recursively packs every [regular file] under dir into one zip (preserving the relative directory structure) and returns the raw zip bytes.
// Empty directory / no files in it at all → returns ok=false (the caller then falls back to a "text-only delivery").
// All regular files are collected (empty ones included); hidden files (dot-prefixed) are collected too — whatever the alter put there gets sent.
func ZipArtifactDir(dir string) (raw []byte, ok bool, err error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	n := 0
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		// Regular files only (skip symlinks / devices etc.).
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		// Always use forward slashes inside the zip.
		name := filepath.ToSlash(rel)
		fw, cerr := zw.Create(name)
		if cerr != nil {
			return cerr
		}
		data, ferr := os.ReadFile(p)
		if ferr != nil {
			return ferr
		}
		if _, werr := fw.Write(data); werr != nil {
			return werr
		}
		n++
		return nil
	})
	if walkErr != nil {
		_ = zw.Close()
		return nil, false, walkErr
	}
	if err := zw.Close(); err != nil {
		return nil, false, err
	}
	if n == 0 {
		return nil, false, nil
	}
	return buf.Bytes(), true, nil
}

// ZipArtifactPath zips one deliverable path (either a single file or a whole delivery directory) into one archive.
//   - path is a directory → recursively pack every file under it (preserving the relative structure).
//   - path is a single file → zip it too (per the "zip everything" policy a single file is zipped as well, so the UI always shows one attachment).
//
// Returns the raw zip bytes; nothing to pack (empty directory / empty file) → ok=false.
func ZipArtifactPath(path string) (raw []byte, ok bool, err error) {
	info, serr := os.Stat(path)
	if serr != nil {
		return nil, false, serr
	}
	if info.IsDir() {
		return ZipArtifactDir(path)
	}
	// Single file: build a zip containing only that file (entry name = the file name itself).
	data, ferr := os.ReadFile(path)
	if ferr != nil {
		return nil, false, ferr
	}
	if len(data) == 0 {
		return nil, false, nil
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	fw, cerr := zw.Create(filepath.Base(path))
	if cerr != nil {
		_ = zw.Close()
		return nil, false, cerr
	}
	if _, werr := fw.Write(data); werr != nil {
		_ = zw.Close()
		return nil, false, werr
	}
	if err := zw.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// DeliveryZipName is the outward file name of the delivery zip: "交付物-<missionID>.zip" (the prefix is the user-facing word for "deliverable").
func DeliveryZipName(missionID string) string {
	id := SanitizeID(missionID)
	if id == "" {
		id = "delivery"
	}
	return "交付物-" + id + ".zip"
}

// Large-file chunked transfer: gets past the relay's ~1MB per-envelope cap.
//
// Threshold: a raw file ≤ MaxArtifactBytes (700KB) still takes the original inline path (Artifact sent
// directly with the message) and this chunking logic is not involved at all. Only > 700KB is chunked.
//
// ChunkRawBytes is the raw byte size of each chunk: 512KB → about 683KB after base64, which plus
// envelope/JSON overhead stays comfortably below the relay's 1MB per-envelope cap. Not a single relay line changes.
const ChunkRawBytes = 512 * 1024

// NewArtifactID generates the unique ID of one file transfer (16 random bytes → hex; file-name/URL safe).
// Uses crypto/rand, no new dependency. It is both the receiver's reassembly key and the "msgID" used when finally written to disk.
func NewArtifactID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand failure is extremely rare; fall back to a timestamp, still non-empty and unique.
		return "art-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}

// ShouldChunk reports whether raw bytes of this size must go through chunked transfer (> cap means yes).
func ShouldChunk(size int) bool { return size > MaxArtifactBytes }

// SplitChunks splits raw bytes into ChunkRawBytes-sized chunks and returns them (not base64; the caller encodes when sending).
// The last chunk may be shorter. Empty input returns nil.
func SplitChunks(raw []byte) [][]byte { return SplitChunksOf(raw, ChunkRawBytes) }

// SplitChunksOf is SplitChunks with an explicit chunk size (GroupChunkRawBytes for group
// fan-out). size <= 0 falls back to ChunkRawBytes.
func SplitChunksOf(raw []byte, size int) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	if size <= 0 {
		size = ChunkRawBytes
	}
	var out [][]byte
	for i := 0; i < len(raw); i += size {
		end := i + size
		if end > len(raw) {
			end = len(raw)
		}
		out = append(out, raw[i:end])
	}
	return out
}

// ChunkTotal computes how many chunks the given byte count needs (ceil(size/ChunkRawBytes)).
func ChunkTotal(size int) int {
	if size <= 0 {
		return 0
	}
	return (size + ChunkRawBytes - 1) / ChunkRawBytes
}

// SHA256Hex computes the hex sha256 of the whole file (compared after reassembly for integrity).
func SHA256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ——— Group attachments (§14.8) ———
//
// A group post travels through one more base64 layer than a pairwise letter: the inner
// message JSON (attachment base64) is sealed into the cipher blob `{"e","i","c"}` whose
// `c` is base64, and the blob itself is base64 again in Envelope.cipher. Raw bytes thus
// grow by (4/3)^3 ≈ 2.37× on the wire, so the pairwise thresholds would overflow the
// relay's 1 MiB body limit on POST /group/mail. The group path uses its own, smaller
// numbers: 384 KiB raw → about 932 KB on the wire, leaving room for the envelope.
const (
	// GroupMaxArtifactBytes is the raw-size cap for a group attachment sent inline.
	GroupMaxArtifactBytes = 384 * 1024
	// GroupChunkRawBytes is the raw size of one group artifact_chunk.
	GroupChunkRawBytes = 384 * 1024
)

// GroupShouldChunk reports whether a group attachment of this raw size must be chunked.
func GroupShouldChunk(size int) bool { return size > GroupMaxArtifactBytes }

// MaxArtifactNameLen caps an attachment file name (bytes).
const MaxArtifactNameLen = 200

// ValidArtifactName reports whether name is safe to use as the file-name part of an
// on-disk attachment (a2a/artifacts/<peer>/<key>__<name>): non-empty, at most
// MaxArtifactNameLen bytes, not "." / "..", no path separators, no ".." sequence, no
// drive / stream colon, no control characters. The name comes from the sender and must
// never be able to steer the write outside the artifacts directory.
func ValidArtifactName(name string) bool {
	if name == "" || len(name) > MaxArtifactNameLen || name == "." || strings.Contains(name, "..") {
		return false
	}
	if strings.ContainsAny(name, `/\:`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ——— Sender side: deliver one large file in chunks ———

// sendChunkedArtifact delivers one large file to fr in chunks:
//  1. first send a "chunk announcement" message (announce, whose Type/Body/Task etc. the caller has already
//     built; here only ArtifactID/Name/ChunkTotal/SHA/Size are filled in, and Artifact is cleared — no bytes).
//  2. then send N artifact_chunk messages in order (each with this chunk's base64 + the same metadata; self-contained).
//  3. also keep a local copy of the whole file at artifactPath(peer, artifactID, name) so our own UI can download it.
//
// Any failed send automatically enters the outbox for retry (reusing sendMessage), so when chunks arrive out of
// order / late the receiver reassembles idempotently by index; this side need not guarantee in-order delivery.
