package relay

import (
	"errors"
	"testing"
)

// No guard installed: a takeover works exactly as before (this is what every existing
// deployment and every old client sees).
func TestClaimGuardNilKeepsTakeover(t *testing.T) {
	f := newDeviceFixture(t)
	if code, body := f.claim(t, "dev-A", "Desk"); code != 200 {
		t.Fatalf("claim A: %d %v", code, body)
	}
	if code, body := f.claim(t, "dev-B", "Laptop"); code != 200 {
		t.Fatalf("takeover by B without a guard must pass: %d %v", code, body)
	}
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-B" {
		t.Fatalf("B must be active: %+v", ad)
	}
	// Installing and then removing a guard restores the default as well.
	f.s.SetClaimGuard(func(box, device string) error { return errors.New("no") })
	f.s.SetClaimGuard(nil)
	if code, body := f.claim(t, "dev-A", "Desk"); code != 200 {
		t.Fatalf("takeover after removing the guard must pass: %d %v", code, body)
	}
}

// A guard that refuses answers 409 with its code and fields and changes nothing.
func TestClaimGuardRefuses(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.claim(t, "dev-A", "Desk"); code != 200 {
		t.Fatal("claim A")
	}
	var events int
	f.s.Subscribe(func(ev Event) {
		if ev.Kind == EventBoxActiveChanged {
			events++
		}
	})
	var gotBox, gotDevice string
	f.s.SetClaimGuard(func(box, device string) error {
		gotBox, gotDevice = box, device
		if device == "dev-A" {
			return nil
		}
		// The guard runs outside the active-device lock: reading it must not deadlock.
		ad := f.s.ActiveDevice(box)
		return &ClaimDenied{Code: "pinned", Fields: map[string]any{"pinned_device": ad.Device, "error": "ignored"}}
	})
	code, body := f.claim(t, "dev-B", "Laptop")
	if code != 409 || body["error"] != "pinned" || body["pinned_device"] != "dev-A" {
		t.Fatalf("refused claim must be 409 pinned: %d %v", code, body)
	}
	if gotBox != f.box || gotDevice != "dev-B" {
		t.Fatalf("guard got (%q,%q)", gotBox, gotDevice)
	}
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-A" {
		t.Fatalf("A must still be active: %+v", ad)
	}
	// The holder is still served (guard allows it), and B is still kicked on its mailbox.
	if code, body := f.claim(t, "dev-A", "Desk 2"); code != 200 {
		t.Fatalf("holder re-claim: %d %v", code, body)
	}
	code, body = f.poll(t, "dev-B")
	assertKicked(t, code, body, "dev-A")
	if events != 0 {
		t.Fatalf("no active-changed event may be emitted: %d", events)
	}

	// A plain error maps to the generic code.
	f.s.SetClaimGuard(func(box, device string) error { return errors.New("maintenance") })
	code, body = f.claim(t, "dev-B", "Laptop")
	if code != 409 || body["error"] != ClaimDeniedErrorCode || body["message"] != "maintenance" {
		t.Fatalf("plain error verdict: %d %v", code, body)
	}
}

// A guard that allows lets the takeover through unchanged (event, new active device).
func TestClaimGuardAllows(t *testing.T) {
	f := newDeviceFixture(t)
	if code, _ := f.claim(t, "dev-A", "Desk"); code != 200 {
		t.Fatal("claim A")
	}
	calls := 0
	f.s.SetClaimGuard(func(box, device string) error { calls++; return nil })
	if code, body := f.claim(t, "dev-B", "Laptop"); code != 200 {
		t.Fatalf("allowed takeover: %d %v", code, body)
	}
	if ad := f.s.ActiveDevice(f.box); ad == nil || ad.Device != "dev-B" {
		t.Fatalf("B must be active: %+v", ad)
	}
	code, body := f.poll(t, "dev-A")
	assertKicked(t, code, body, "dev-B")
	if calls != 1 {
		t.Fatalf("guard must be consulted once per explicit claim: %d", calls)
	}
}
