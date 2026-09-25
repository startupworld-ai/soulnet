package peer

import (
	"context"
	"errors"

	"github.com/startupworld-ai/soulnet/a2a"
)

// ——— vault: encrypted, content-addressed backup storage on our relay ———
//
// Thin forwarding to a2a's vault client (see a2a/vault.go for the model and the refs blob
// format, relay/vault.go for the rules). The peer neither derives ids nor encrypts: the
// host hands it ciphertext and keyed-hash ids. Requests carry DeviceID / DeviceName, so
// lane main answers *ErrKicked on a non-active device, exactly like the mailbox.

// Vault types and sentinels, re-exported so hosts need not import a2a for them.
type (
	VaultHead          = a2a.VaultHead
	VaultUsage         = a2a.VaultUsage
	VaultConflictError = a2a.VaultConflictError
	VaultMissingError  = a2a.VaultMissingError
)

// Vault sentinels (errors.Is). These verdicts are returned as is, not wrapped in
// ErrNetwork, so a host can tell "the relay said no" from "the relay is unreachable".
var (
	ErrVaultConflict    = a2a.ErrVaultConflict
	ErrVaultQuota       = a2a.ErrVaultQuota
	ErrVaultMissing     = a2a.ErrVaultMissing
	ErrVaultNotFound    = a2a.ErrVaultNotFound
	ErrVaultUnsupported = a2a.ErrVaultUnsupported
)

// VaultMainLane is the lane only the active device may advance.
const VaultMainLane = a2a.VaultMainLane

// VaultDevLane returns this device's own lane ("dev-" + DeviceID; "" without a DeviceID).
func (n *Peer) VaultDevLane() string {
	if n.DeviceID == "" {
		return ""
	}
	return a2a.VaultDevLane(n.DeviceID)
}

// vaultWrap returns the vault verdicts (sentinels above) and *ErrKicked as they are and
// wraps everything else in ErrNetwork like the other relay calls (a *a2a.RelayError, e.g.
// the 403 of another device's dev lane, stays reachable through errors.As).
func vaultWrap(err error) error {
	if err == nil {
		return nil
	}
	for _, s := range []error{ErrVaultConflict, ErrVaultQuota, ErrVaultMissing, ErrVaultNotFound, ErrVaultUnsupported} {
		if errors.Is(err, s) {
			return err
		}
	}
	return wrapNet(err)
}

// vaultClient returns the signed client for our relay (nil without an identity).
func (n *Peer) vaultClient() *a2a.ProxyClient { return n.proxyClient() }

// VaultPut stores blob id (ciphertext, at most a2a.MaxVaultBlobBytes) on our relay;
// idempotent, existed reports the relay already had it. ErrVaultQuota when full.
func (n *Peer) VaultPut(ctx context.Context, id string, data []byte) (existed bool, err error) {
	pc := n.vaultClient()
	if pc == nil {
		return false, ErrNoIdentity
	}
	existed, err = pc.VaultPut(ctxOrBackground(ctx), id, data)
	return existed, vaultWrap(err)
}

// VaultHas returns the ids our relay does not store (input order; any number of ids).
func (n *Peer) VaultHas(ctx context.Context, ids []string) ([]string, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	missing, err := pc.VaultHas(ctxOrBackground(ctx), ids)
	return missing, vaultWrap(err)
}

// VaultGet returns blob id from our relay (ErrVaultNotFound when not stored).
func (n *Peer) VaultGet(ctx context.Context, id string) ([]byte, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	data, err := pc.VaultGet(ctxOrBackground(ctx), id)
	return data, vaultWrap(err)
}

// VaultHead returns the current version of lane (nil, nil when the lane does not exist).
func (n *Peer) VaultHead(ctx context.Context, lane string) (*VaultHead, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	h, err := pc.VaultHead(ctxOrBackground(ctx), lane)
	return h, vaultWrap(err)
}

// VaultHeads returns every lane of our mailbox's vault, sorted by lane.
func (n *Peer) VaultHeads(ctx context.Context) ([]VaultHead, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	hs, err := pc.VaultHeads(ctxOrBackground(ctx))
	return hs, vaultWrap(err)
}

// VaultSetHead advances lane from prevVersion (0 = create) to prevVersion+1 = {root, refs}.
// *VaultConflictError / *VaultMissingError / *ErrKicked / 403 as in a2a.ProxyClient.VaultSetHead.
func (n *Peer) VaultSetHead(ctx context.Context, lane string, prevVersion int64, root, refs string) (*VaultHead, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	h, err := pc.VaultSetHead(ctxOrBackground(ctx), lane, prevVersion, root, refs)
	return h, vaultWrap(err)
}

// VaultDeleteHead drops lane if it is at prevVersion (a missing lane is not an error).
func (n *Peer) VaultDeleteHead(ctx context.Context, lane string, prevVersion int64) error {
	pc := n.vaultClient()
	if pc == nil {
		return ErrNoIdentity
	}
	return vaultWrap(pc.VaultDeleteHead(ctxOrBackground(ctx), lane, prevVersion))
}

// VaultUsage reports our vault's bytes / blobs / quota on our relay.
func (n *Peer) VaultUsage(ctx context.Context) (*VaultUsage, error) {
	pc := n.vaultClient()
	if pc == nil {
		return nil, ErrNoIdentity
	}
	u, err := pc.VaultUsage(ctxOrBackground(ctx))
	return u, vaultWrap(err)
}
