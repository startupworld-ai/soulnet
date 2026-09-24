package relay

import (
	"strings"
	"testing"

	"github.com/startupworld-ai/soulnet/a2a"
)

// The claimer may leave an opaque handoff note; the kicked device receives it verbatim in
// the 409 verdict and GET /box/active shows it to the owner. The relay never interprets it.
func TestDeviceHandoffTravelsToTheKickedDevice(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.claim(t, "dev-A", "Office desktop"); code != 200 {
		t.Fatalf("first claim: %d", code)
	}
	note := "enc:rendezvous=abc123;key=..."
	code, body := f.do(t, "POST", "/box/active", "", "dev-B", map[string]any{
		"box": f.box, "device": "dev-B", "name": "Phone", "handoff": note,
	})
	if code != 200 {
		t.Fatalf("claim with handoff: %d %v", code, body)
	}
	code, body = f.poll(t, "dev-A")
	assertKicked(t, code, body, "dev-B")
	if got, _ := body["handoff"].(string); got != note {
		t.Fatalf("kicked verdict should carry the handoff note verbatim, got %q", got)
	}
	code, body = f.do(t, "GET", "/box/active", "?box="+f.box, "dev-B", nil)
	if code != 200 {
		t.Fatalf("GET /box/active: %d %v", code, body)
	}
	active, _ := body["active"].(map[string]any)
	if got, _ := active["handoff"].(string); got != note {
		t.Fatalf("GET /box/active should show the handoff, got %q", got)
	}
	// Re-claim by the holder with a fresh note replaces it; an empty note clears it.
	if code, _ := f.do(t, "POST", "/box/active", "", "dev-B", map[string]any{"box": f.box, "device": "dev-B", "handoff": "second"}); code != 200 {
		t.Fatalf("re-claim: %d", code)
	}
	if code, body = f.poll(t, "dev-A"); body["handoff"] != "second" {
		t.Fatalf("re-claim should replace the note, got %v (code %d)", body["handoff"], code)
	}
	// Oversize notes are refused.
	code, _ = f.do(t, "POST", "/box/active", "", "dev-B", map[string]any{"box": f.box, "device": "dev-B", "handoff": strings.Repeat("x", a2a.MaxHandoffBytes+1)})
	if code != 400 {
		t.Fatalf("oversize handoff should be 400, got %d", code)
	}
}
