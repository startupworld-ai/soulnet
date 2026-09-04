package peer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// The helpers a host embedding the peer behind its own receive loop relies on.

func TestPermanentClassification(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must stay nil")
	}
	base := fmt.Errorf("decryption failed")
	p := Permanent(base)
	if !IsPermanent(p) || IsPermanent(base) || IsPermanent(nil) {
		t.Fatal("only errors wrapped by Permanent classify as permanent")
	}
	if !IsPermanent(fmt.Errorf("outer: %w", p)) {
		t.Fatal("wrapping a permanent error must keep the classification")
	}
	if !errors.Is(p, base) || p.Error() != base.Error() {
		t.Fatal("Permanent must unwrap to and read as the underlying error")
	}
}

func TestNoteDeadLetterLogsOncePerAck(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "home"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	n.Logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	e := Permanent(fmt.Errorf("bad cipher"))
	n.NoteDeadLetter("ack-1", e)
	n.NoteDeadLetter("ack-1", e)
	n.NoteDeadLetter("ack-2", e)
	if len(lines) != 2 {
		t.Fatalf("want one log line per distinct ack id, got %d: %v", len(lines), lines)
	}
}

func TestSanitizeVoices(t *testing.T) {
	in := []string{" DevBot ", "", "has space", "at@sign", strings.Repeat("x", 65), "ok"}
	got := SanitizeVoices(in)
	if len(got) != 2 || got[0] != "DevBot" || got[1] != "ok" {
		t.Fatalf("unexpected sanitized list: %v", got)
	}
	many := make([]string, 40)
	for i := range many {
		many[i] = fmt.Sprintf("A%02d", i)
	}
	if got := SanitizeVoices(many); len(got) != 16 {
		t.Fatalf("want the list capped at 16, got %d", len(got))
	}
}

func TestOpenEnvelopeClassifiesPoisonMail(t *testing.T) {
	n, err := Init(filepath.Join(t.TempDir(), "home"), "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	n.Logf = func(string, ...any) {}
	if _, err := n.EnsureIdentity("me"); err != nil {
		t.Fatal(err)
	}
	stranger, err := a2a.NewIdentity(t.TempDir(), "stranger", []string{"http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	myCard, _ := n.Card()
	msg := &a2a.Message{ID: "m1", From: stranger.Fingerprint(), To: n.Fingerprint(), Type: a2a.TypeText, Body: "hi"}
	env, err := a2a.SealEnvelope(stranger, myCard, msg)
	if err != nil {
		t.Fatal(err)
	}
	// Not a friend, no from_xpub, not a group co-member: undecryptable forever.
	env.FromXPub = ""
	if _, err := n.OpenEnvelope(env); err == nil || !IsPermanent(err) {
		t.Fatalf("unknown sender without xpub must be permanent, got %v", err)
	}
	// With the declared key it opens (first contact of a stranger).
	env.FromXPub = stranger.XPub
	if got, err := n.OpenEnvelope(env); err != nil || got.Body != "hi" || got.From != stranger.Fingerprint() {
		t.Fatalf("declared xpub must open: %v %+v", err, got)
	}
	// Tampered ciphertext: permanent.
	env.Cipher = "AAAA" + env.Cipher[4:]
	if _, err := n.OpenEnvelope(env); err == nil || !IsPermanent(err) {
		t.Fatalf("tampered cipher must be permanent, got %v", err)
	}
	// A peer without an identity refuses.
	bare, _ := Init(filepath.Join(t.TempDir(), "home2"), "http://127.0.0.1:1")
	if _, err := bare.OpenEnvelope(env); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("no identity: want ErrNoIdentity, got %v", err)
	}
}
