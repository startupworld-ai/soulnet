package a2a

import "strings"

// SanitizeID squashes an arbitrary string into a file-name / URL-safe ID (keeps only letters, digits, - and _; everything else becomes _).
// Conversations / missions / attachments etc. all go through it when written to disk under a peer fingerprint or message ID, preventing path traversal.
func SanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// ShortFp takes the first 8 characters of a fingerprint for logs / display.
func ShortFp(fp string) string {
	if len(fp) > 8 {
		return fp[:8]
	}
	return fp
}

// MaxArtifactBytes is the raw-size cap for attachments sent inline with a pairwise message.
// Anything larger goes through chunked transfer (see chunk.go).
//
// A pairwise attachment is base64'd twice on its way into POST /mail: once inside the
// message JSON (Message.artifact) and once more when the sealed ciphertext becomes
// Envelope.cipher. Raw bytes thus grow by (4/3)^2 ≈ 1.78x on the wire, and the relay reads
// at most 1 MiB of body. 512 KiB raw → about 932 KB on the wire, leaving ~100 KB for the
// message text, metadata and the envelope. (The old 700 KiB cap came to ~1.24 MB on the
// wire: every inline file between ~575 KiB and 700 KiB was rejected with HTTP 400.)
// It equals ChunkRawBytes, so an inline file and one chunk have the same worst case.
const MaxArtifactBytes = 512 * 1024

// ControlMarkers are [internal signalling] control markers that must never leak to the peer or land in the conversation archive / chat UI.
var ControlMarkers = []string{"END_OF_CONVERSATION", "END OF CONVERSATION"}

// StripControlMarkers removes every internal control marker from a message body (case-insensitive, wherever it
// appears as a substring) and cleans up the extra whitespace/blank lines left behind. Every body goes through it before
// entering the conversation archive / display / outbound send, guaranteeing internal signalling never leaks. The body
// may become empty afterwards; the caller then wraps up as "no reply needed".
func StripControlMarkers(body string) string {
	if body == "" {
		return body
	}
	for _, m := range ControlMarkers {
		for {
			i := indexFoldASCII(body, m)
			if i < 0 {
				break
			}
			body = body[:i] + body[i+len(m):]
		}
	}
	lines := strings.Split(body, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// indexFoldASCII returns the byte index of the first occurrence of sub in s (ASCII case-insensitive), or -1 if none.
// sub must be ASCII, so the comparison can fold byte by byte without being affected by multi-byte characters in s.
func indexFoldASCII(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if equalFoldASCII(s[i:i+m], sub) {
			return i
		}
	}
	return -1
}

// equalFoldASCII compares two equal-length strings byte by byte, treating ASCII letters case-insensitively.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
