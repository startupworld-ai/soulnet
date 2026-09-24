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
