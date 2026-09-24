// ProxyClient methods for the relay's device-session endpoints (active device of a mailbox,
// pairing rendezvous). Thin wire wrappers: no encryption happens here -- what goes through
// a rendezvous is sealed by the caller.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ClaimActive makes this client's Device the active device of our mailbox
// (POST /box/active, owner-signed): whatever device held it before is kicked on its next
// mailbox request (its long poll is woken at once). Requires WithDevice.
func (c *ProxyClient) ClaimActive(ctx context.Context, handoff string) (*ActiveDevice, error) {
	if c.Device == "" {
		return nil, fmt.Errorf("ClaimActive: no device id configured (WithDevice)")
	}
	if c.id == nil {
		return nil, fmt.Errorf("ClaimActive: no identity")
	}
	if len(handoff) > MaxHandoffBytes {
		return nil, fmt.Errorf("ClaimActive: handoff exceeds %d bytes", MaxHandoffBytes)
	}
	body, _ := json.Marshal(map[string]any{"box": c.id.Fingerprint(), "device": c.Device, "name": c.DeviceName, "handoff": handoff})
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/box/active", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.signGet(req, "POST", "/box/active"); err != nil {
		return nil, err
	}
	c.setDevice(req)
	resp, err := c.shortHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apiErr(resp)
	}
	var out struct {
		Active *ActiveDevice `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Active, nil
}

// ActiveDevice reports which device currently holds our mailbox (GET /box/active,
// owner-signed); nil when no device has ever claimed it.
func (c *ProxyClient) ActiveDevice(ctx context.Context) (*ActiveDevice, error) {
	if c.id == nil {
		return nil, fmt.Errorf("ActiveDevice: no identity")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/box/active?box="+url.QueryEscape(c.id.Fingerprint()), nil)
	if err != nil {
		return nil, err
	}
	if err := c.signGet(req, "GET", "/box/active"); err != nil {
		return nil, err
	}
	resp, err := c.shortHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apiErr(resp)
	}
	var out struct {
		Active *ActiveDevice `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Active, nil
}

// RendezvousPut stores one blob at rendezvous id (POST /rendezvous/{id}; no auth, data
// must already be ciphertext, at most 4 MB).
func (c *ProxyClient) RendezvousPut(ctx context.Context, id string, seq int64, data []byte) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("rendezvous id must not be empty")
	}
	body, _ := json.Marshal(RendezvousItem{Seq: seq, Data: data})
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/rendezvous/"+url.PathEscape(id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.shortHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apiErr(resp)
	}
	return nil
}

// RendezvousGet returns the blobs stored at id with seq > since, in seq order
// (GET /rendezvous/{id}?since=&wait=). waitSec > 0 long-polls (the relay caps it at 55 s)
// using the long-poll HTTP client; an unknown rendezvous yields an empty list, not an error.
func (c *ProxyClient) RendezvousGet(ctx context.Context, id string, since int64, waitSec int) ([]RendezvousItem, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("rendezvous id must not be empty")
	}
	u := fmt.Sprintf("%s/rendezvous/%s?since=%d&wait=%d", c.Base, url.PathEscape(id), since, waitSec)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	hc := c.shortHTTP()
	if waitSec > 0 {
		hc = c.HTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apiErr(resp)
	}
	var out struct {
		Items []RendezvousItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// RendezvousDelete drops rendezvous id and everything stored at it (DELETE /rendezvous/{id}; idempotent).
func (c *ProxyClient) RendezvousDelete(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("rendezvous id must not be empty")
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.Base+"/rendezvous/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := c.shortHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apiErr(resp)
	}
	return nil
}
