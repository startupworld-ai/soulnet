package a2a

import (
	"fmt"
	"time"
)

// ——— Device sessions (one identity, many devices, one active at a time) ———
//
// A key pair may live on several machines (a desktop, a laptop, a phone); the relay lets
// exactly one of them -- the ACTIVE device -- touch the mailbox (poll, ack, send as this
// identity). The device id is NOT a credential: requests are still signed with the
// identity key, the id only tells the relay which of the identity's devices is talking.

// Request headers of the device session.
const (
	// HeaderDevice carries the caller's device id (an opaque token the device generated for
	// itself; 16 random bytes base64url is the convention). Absent on legacy clients.
	HeaderDevice = "X-Soulnet-Device"
	// HeaderDeviceName optionally carries a human-readable device name ("Office desktop").
	// The relay stores it only when the request implicitly claims the mailbox (first
	// device to show up on a mailbox without an active device); an explicit claim through
	// POST /box/active carries the name in its body instead.
	HeaderDeviceName = "X-Soulnet-Device-Name"
)

// ActiveDevice is the device currently holding a mailbox, as the relay reports it.
type ActiveDevice struct {
	Device string    `json:"device"`
	Name   string    `json:"name,omitempty"`
	Since  time.Time `json:"since"`
	// Handoff is an opaque blob (<= 4 KB) the claiming device leaves for the device it
	// kicks: typically an encrypted "upload your latest state to rendezvous X with key K"
	// note, so the previous device can hand its data over the moment it learns it was
	// kicked. The relay stores and relays it verbatim (409 kicked carries it); it never
	// interprets it. Empty when the claimer left nothing.
	Handoff string `json:"handoff,omitempty"`
}

// DeviceSeen is one device of an identity as the relay last saw it (GET /box/devices):
// every owner-signed request carrying HeaderDevice, and the POST /box/seen heartbeat a
// frozen device sends, refresh LastSeen. Active marks the mailbox's active device.
type DeviceSeen struct {
	Device   string    `json:"device"`
	Name     string    `json:"name,omitempty"`
	LastSeen time.Time `json:"last_seen"`
	Active   bool      `json:"active,omitempty"`
	// Offline is set when the device said goodbye (POST /box/seen {offline:true}) on a clean
	// shutdown and has not talked to the relay since: a peer can stop waiting for it at once
	// instead of inferring "gone" from LastSeen growing old. Any later signed request clears it.
	Offline bool `json:"offline,omitempty"`
}

// RendezvousItem is one blob stored at a pairing rendezvous (POST/GET /rendezvous/{id}).
// Data is opaque ciphertext to the relay; JSON carries it base64 (standard encoding).
type RendezvousItem struct {
	Seq  int64  `json:"seq"`
	Data []byte `json:"data"`
}

// ErrKicked is the relay's verdict that ANOTHER device of this identity is the active one
// (HTTP 409 {"error":"kicked", ...}). The caller must stop touching the mailbox -- no retry
// helps until the user claims the mailbox on this device again. Test with errors.As:
//
//	var k *a2a.ErrKicked
//	if errors.As(err, &k) { ... k.ActiveDevice ... }
type ErrKicked struct {
	ActiveDevice string
	ActiveName   string
	Since        time.Time
	// Handoff is the claimer's opaque note (see ActiveDevice.Handoff); the kicked device
	// uses it to hand its latest state over before freezing. Empty when none was left.
	Handoff string
}

func (e *ErrKicked) Error() string {
	who := e.ActiveDevice
	if e.ActiveName != "" {
		who = fmt.Sprintf("%s (%s)", e.ActiveName, e.ActiveDevice)
	}
	return fmt.Sprintf("kicked: another device is active on this mailbox: %s since %s", who, e.Since.Format(time.RFC3339))
}

// KickedErrorCode is the JSON "error" value of the relay's 409 kicked verdict:
// {"error":"kicked","active_device":…,"active_name":…,"since":…}. Both ends key on it.
const KickedErrorCode = "kicked"

// MaxHandoffBytes caps ActiveDevice.Handoff (the claimer's opaque note to the kicked
// device). 4 KB is plenty for "rendezvous id + wrapped key"; it is not a data channel.
const MaxHandoffBytes = 4096
