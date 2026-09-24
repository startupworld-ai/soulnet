package peer

import (
	"context"
	"fmt"

	"github.com/startupworld-ai/soulnet/a2a"
)

// ——— device sessions: one identity on several devices, one active at a time ———
//
// The peer only speaks the wire protocol here (see a2a/device.go and relay/active.go for
// the rule). What a host does when it is kicked -- freeze, show a banner, offer "use the
// identity on this device" -- is the host's business; the peer stops its loop, emits
// device.kicked and waits to be restarted after ClaimActive.

// ClaimActive makes this device (DeviceID) the active device of our mailbox on our relay:
// whatever device held it before is kicked at once. The host restarts Run afterwards if
// the loop had stopped. Requires an identity and a DeviceID.
func (n *Peer) ClaimActive(ctx context.Context) (*a2a.ActiveDevice, error) {
	if n.DeviceID == "" {
		return nil, fmt.Errorf("ClaimActive: DeviceID is not set")
	}
	pc := n.proxyClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	ad, err := pc.ClaimActive(ctxOrBackground(ctx))
	if err != nil {
		return nil, wrapNet(err)
	}
	n.logf("claimed the mailbox on this device (%s)", n.DeviceID)
	return ad, nil
}

// ActiveDevice reports which device holds our mailbox on our relay; nil when no device has
// ever claimed it (a mailbox only legacy clients have used).
func (n *Peer) ActiveDevice(ctx context.Context) (*a2a.ActiveDevice, error) {
	pc := n.proxyClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	ad, err := pc.ActiveDevice(ctxOrBackground(ctx))
	if err != nil {
		return nil, wrapNet(err)
	}
	return ad, nil
}

// IsActiveHere reports whether this device may use the mailbox right now: no device has
// claimed it yet, or the holder is us. Requires a DeviceID (false without one).
func (n *Peer) IsActiveHere(ctx context.Context) (bool, error) {
	if n.DeviceID == "" {
		return false, nil
	}
	ad, err := n.ActiveDevice(ctx)
	if err != nil {
		return false, err
	}
	return ad == nil || ad.Device == n.DeviceID, nil
}

// rendezvousClient talks to our relay without needing an identity: a device that is about
// to JOIN the identity has none yet, and the rendezvous endpoints are unauthenticated.
func (n *Peer) rendezvousClient() *a2a.ProxyClient {
	return a2a.NewProxyClient(n.RelayBase(), n.Identity()).WithDeliverTimeout(DeliverTimeout)
}

// RendezvousPut stores one blob (already encrypted by the caller; at most 4 MB) at
// rendezvous id on our relay under sequence number seq.
func (n *Peer) RendezvousPut(ctx context.Context, id string, seq int64, data []byte) error {
	return wrapNet(n.rendezvousClient().RendezvousPut(ctxOrBackground(ctx), id, seq, data))
}

// RendezvousGet returns the blobs at rendezvous id with seq > since, in order; waitSec > 0
// long-polls (relay cap 55 s). An unknown rendezvous yields an empty list.
func (n *Peer) RendezvousGet(ctx context.Context, id string, since int64, waitSec int) ([]a2a.RendezvousItem, error) {
	items, err := n.rendezvousClient().RendezvousGet(ctxOrBackground(ctx), id, since, waitSec)
	if err != nil {
		return nil, wrapNet(err)
	}
	return items, nil
}

// RendezvousDelete drops rendezvous id on our relay (idempotent).
func (n *Peer) RendezvousDelete(ctx context.Context, id string) error {
	return wrapNet(n.rendezvousClient().RendezvousDelete(ctxOrBackground(ctx), id))
}

// wrapNet wraps a relay failure in ErrNetwork (host error-code mapping), keeping the
// relay's own verdicts (*a2a.RelayError status, *ErrKicked) reachable through errors.As.
func wrapNet(err error) error {
	if err == nil {
		return nil
	}
	if asKicked(err) != nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrNetwork, err)
}
