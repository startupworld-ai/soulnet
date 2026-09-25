// ProxyClient methods for the relay's vault endpoints (relay/vault.go). Thin wire
// wrappers: blob ids and ciphertext are computed by the caller; nothing is encrypted here.
// Every request is owner-signed and carries the device headers (WithDevice), so lane main
// answers *ErrKicked on a non-active device like the mailbox does.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultVaultTimeout bounds one vault request (a 4 MiB blob on a slow uplink needs more
// than the 15 s short-request budget).
const DefaultVaultTimeout = 2 * time.Minute

// vaultHTTP returns the client for vault requests: VaultHTTP when set, otherwise one with
// DefaultVaultTimeout sharing HTTP's transport.
func (c *ProxyClient) vaultHTTP() *http.Client {
	if c.VaultHTTP != nil {
		return c.VaultHTTP
	}
	var tr http.RoundTripper
	if c.HTTP != nil {
		tr = c.HTTP.Transport
	}
	return &http.Client{Timeout: DefaultVaultTimeout, Transport: tr}
}

// vaultPath is the signed path of a vault route for our mailbox.
func (c *ProxyClient) vaultPath(suffix string) string {
	return "/vault/" + c.id.Fingerprint() + suffix
}

// vaultDo sends one owner-signed vault request (signature over method + path, query not
// signed) and returns the response when it is 2xx; otherwise the mapped vault error.
func (c *ProxyClient) vaultDo(ctx context.Context, method, path, query string, body []byte, contentType, lane string) (*http.Response, error) {
	if c.id == nil {
		return nil, fmt.Errorf("vault: no identity")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path+query, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if err := c.signGet(req, method, path); err != nil {
		return nil, err
	}
	c.setDevice(req)
	resp, err := c.vaultHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, vaultErr(resp, lane)
	}
	return resp, nil
}

// vaultErr maps a non-2xx vault reply onto the vault sentinels (see vault.go), falling
// back to *ErrKicked / *RelayError. A 404/405 that is not one of the vault's own verdicts
// means the relay predates the vault: ErrVaultUnsupported.
func vaultErr(resp *http.Response, lane string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var e struct {
		Error   string   `json:"error"`
		Version int64    `json:"version"`
		Root    string   `json:"root"`
		Missing []string `json:"missing"`
		Count   int      `json:"count"`
	}
	isJSON := json.Unmarshal(raw, &e) == nil
	base := apiErrFrom(resp.StatusCode, raw)
	switch {
	case resp.StatusCode == http.StatusConflict && e.Error == VaultConflictCode:
		return &VaultConflictError{Lane: lane, Current: e.Version, Root: e.Root}
	case resp.StatusCode == http.StatusRequestEntityTooLarge && e.Error == VaultQuotaCode:
		return fmt.Errorf("%w (%w)", ErrVaultQuota, base)
	case resp.StatusCode == http.StatusUnprocessableEntity && e.Error == VaultMissingCode:
		n := e.Count
		if n < len(e.Missing) {
			n = len(e.Missing)
		}
		return &VaultMissingError{IDs: e.Missing, Count: n}
	case resp.StatusCode == http.StatusNotFound && e.Error == VaultNoBlobCode:
		return fmt.Errorf("%w (%w)", ErrVaultNotFound, base)
	case resp.StatusCode == http.StatusNotFound && e.Error == VaultNoLaneCode:
		return base // callers that treat "no lane" as a value check for it themselves
	case (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed) && (!isJSON || e.Error == ""):
		return fmt.Errorf("%w (%w)", ErrVaultUnsupported, base)
	}
	return base
}

// isNoLane reports whether err is the relay's 404 "no such lane" verdict.
func isNoLane(err error) bool {
	var re *RelayError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound && re.Message == VaultNoLaneCode
}

// VaultPut stores one blob under id (64 lowercase hex, the caller's keyed hash of the
// plaintext; data is ciphertext, at most MaxVaultBlobBytes). Idempotent: existed reports
// that the relay already had it. ErrVaultQuota when the mailbox quota is exhausted.
func (c *ProxyClient) VaultPut(ctx context.Context, id string, data []byte) (existed bool, err error) {
	if !ValidVaultID(id) {
		return false, fmt.Errorf("vault: invalid blob id %q", id)
	}
	if len(data) == 0 || len(data) > MaxVaultBlobBytes {
		return false, fmt.Errorf("vault: blob must be 1..%d bytes, got %d", MaxVaultBlobBytes, len(data))
	}
	resp, err := c.vaultDo(ctx, "PUT", c.vaultPath("/blob/"+id), "", data, "application/octet-stream", "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Existed bool `json:"existed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Existed, nil
}

// VaultHas returns the ids the relay does NOT store, in input order. Any number of ids is
// accepted (queried in batches of MaxVaultHasIDs). Present blobs get their grace period
// refreshed, so skipping their upload is safe until the head that references them is set.
func (c *ProxyClient) VaultHas(ctx context.Context, ids []string) (missing []string, err error) {
	missing = []string{}
	for start := 0; start < len(ids); start += MaxVaultHasIDs {
		end := min(start+MaxVaultHasIDs, len(ids))
		body, _ := json.Marshal(map[string]any{"ids": ids[start:end]})
		resp, err := c.vaultDo(ctx, "POST", c.vaultPath("/has"), "", body, "application/json", "")
		if err != nil {
			return nil, err
		}
		var out struct {
			Missing []string `json:"missing"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		missing = append(missing, out.Missing...)
	}
	return missing, nil
}

// VaultGet returns blob id. ErrVaultNotFound when the relay does not store it.
func (c *ProxyClient) VaultGet(ctx context.Context, id string) ([]byte, error) {
	if !ValidVaultID(id) {
		return nil, fmt.Errorf("vault: invalid blob id %q", id)
	}
	resp, err := c.vaultDo(ctx, "GET", c.vaultPath("/blob/"+id), "", nil, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxVaultBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxVaultBlobBytes {
		return nil, fmt.Errorf("vault: blob %s exceeds %d bytes", id, MaxVaultBlobBytes)
	}
	return data, nil
}

// VaultHead returns the current version of lane; nil (and no error) when the lane does not exist.
func (c *ProxyClient) VaultHead(ctx context.Context, lane string) (*VaultHead, error) {
	if !ValidVaultLane(lane) {
		return nil, fmt.Errorf("vault: invalid lane %q", lane)
	}
	resp, err := c.vaultDo(ctx, "GET", c.vaultPath("/head/"+lane), "", nil, "", lane)
	if isNoLane(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h VaultHead
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

// VaultHeads returns the current version of every lane of our mailbox, sorted by lane.
func (c *ProxyClient) VaultHeads(ctx context.Context) ([]VaultHead, error) {
	resp, err := c.vaultDo(ctx, "GET", c.vaultPath("/heads"), "", nil, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Heads []VaultHead `json:"heads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Heads == nil {
		out.Heads = []VaultHead{}
	}
	return out.Heads, nil
}

// VaultSetHead advances lane from prevVersion (0 = create it) to prevVersion+1 pointing at
// manifest blob root and refs blob refs (both, and every id refs names, must be stored).
//
//	*VaultConflictError (errors.Is ErrVaultConflict)  the lane is not at prevVersion
//	*VaultMissingError  (errors.Is ErrVaultMissing)   some referenced blobs are not stored
//	*ErrKicked                                         lane main written by a non-active device
//	*RelayError 403                                    dev-<id> lane of another device
func (c *ProxyClient) VaultSetHead(ctx context.Context, lane string, prevVersion int64, root, refs string) (*VaultHead, error) {
	if !ValidVaultLane(lane) {
		return nil, fmt.Errorf("vault: invalid lane %q", lane)
	}
	body, _ := json.Marshal(map[string]any{"prev_version": prevVersion, "root": root, "refs": refs})
	resp, err := c.vaultDo(ctx, "PUT", c.vaultPath("/head/"+lane), "", body, "application/json", lane)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Head VaultHead `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out.Head, nil
}

// VaultDeleteHead drops lane if it is at prevVersion (CAS, *VaultConflictError otherwise).
// main: active device only; dev-<id>: that device or the active device (after merging).
// Deleting a lane that does not exist is not an error.
func (c *ProxyClient) VaultDeleteHead(ctx context.Context, lane string, prevVersion int64) error {
	if !ValidVaultLane(lane) {
		return fmt.Errorf("vault: invalid lane %q", lane)
	}
	resp, err := c.vaultDo(ctx, "DELETE", c.vaultPath("/head/"+lane), "?prev_version="+strconv.FormatInt(prevVersion, 10), nil, "", lane)
	if isNoLane(err) {
		return nil
	}
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// VaultUsage reports the storage our mailbox's vault occupies and its quota.
func (c *ProxyClient) VaultUsage(ctx context.Context) (*VaultUsage, error) {
	resp, err := c.vaultDo(ctx, "GET", c.vaultPath("/usage"), "", nil, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u VaultUsage
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

// VaultPurge wipes our mailbox's whole vault (every blob and lane; DELETE /vault/{box}).
// Active device only (*ErrKicked otherwise). Idempotent: purging an empty vault succeeds.
func (c *ProxyClient) VaultPurge(ctx context.Context) error {
	resp, err := c.vaultDo(ctx, "DELETE", c.vaultPath(""), "", nil, "", "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
