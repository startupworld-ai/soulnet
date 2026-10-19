// ProxyClient methods for the relay's group endpoints (spec §14.4): publish/fetch the
// signed roster and post group mail for fan-out.
package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// PublishGroup uploads a signed roster to the relay (POST /group/publish). The roster
// signature is the authorization — no extra auth headers needed.
func (c *ProxyClient) PublishGroup(ctx context.Context, roster *GroupRoster) error {
	raw, err := json.Marshal(roster)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/group/publish", bytes.NewReader(raw))
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

// FetchGroup downloads the current roster of one group (GET /group/fetch?gid=). The
// request is signed; the relay answers members only.
func (c *ProxyClient) FetchGroup(ctx context.Context, gid string) (*GroupRoster, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.Base+"/group/fetch?gid="+url.QueryEscape(gid), nil)
	if err != nil {
		return nil, err
	}
	if err := c.signGet(req, "GET", "/group/fetch"); err != nil {
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
		Roster *GroupRoster `json:"roster"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Roster == nil {
		return nil, fmt.Errorf("relay returned no roster")
	}
	return out.Roster, nil
}

// UnpublishGroup takes a roster off the relay (POST /group/unpublish, signed request; the
// relay only honours the roster owner). Used by GroupDissolve.
func (c *ProxyClient) UnpublishGroup(ctx context.Context, gid string) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.Base+"/group/unpublish?gid="+url.QueryEscape(gid), nil)
	if err != nil {
		return err
	}
	if err := c.signGet(req, "POST", "/group/unpublish"); err != nil {
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

// FetchGroupCard downloads the PUBLIC card of a group (GET /group/card, no auth) — what
// a stranger holding a soulmirror://group?... handle uses to find where to apply.
func (c *ProxyClient) FetchGroupCard(ctx context.Context, gid string) (*GroupCard, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.Base+"/group/card?gid="+url.QueryEscape(gid), nil)
	if err != nil {
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
		Card *GroupCard `json:"card"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Card == nil || out.Card.OwnerCard == nil {
		return nil, fmt.Errorf("relay returned no usable group card")
	}
	return out.Card, nil
}

// DeliverGroup posts one group envelope for fan-out (POST /group/mail).
//
// The body can be close to 1 MiB when the post carries an inline attachment or an
// artifact_chunk (§14.8), and the short-request timeout (15s by default) covers the
// whole upload. On a slow uplink the relay received and fanned the mail out while the
// client had already given up — the sender saw "failed", retried, and the group got
// duplicates. So the timeout grows with the body: DeliverTimeout plus one second per
// 16 KiB, capped by GroupDeliverMaxTimeout.
func (c *ProxyClient) DeliverGroup(ctx context.Context, env *Envelope) error {
	raw, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/group/mail", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setDevice(req) // posting AS this identity is gated on the active device, like Deliver
	resp, err := c.groupHTTP(len(raw)).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return apiErr(resp)
	}
	return nil
}

// GroupDeliverMaxTimeout caps the size-scaled timeout of DeliverGroup.
const GroupDeliverMaxTimeout = 2 * time.Minute

// GroupDeliverTimeout is the HTTP timeout DeliverGroup uses for a body of n bytes on top
// of base (the short-request timeout): base + 1s per 16 KiB, never above
// GroupDeliverMaxTimeout and never below base.
func GroupDeliverTimeout(base time.Duration, n int) time.Duration {
	if base <= 0 {
		base = DefaultDeliverTimeout
	}
	d := base + time.Duration(n/(16*1024))*time.Second
	if d > GroupDeliverMaxTimeout {
		d = GroupDeliverMaxTimeout
	}
	return d
}

// groupHTTP returns a client like shortHTTP() (same transport) whose timeout fits a
// /group/mail body of n bytes.
func (c *ProxyClient) groupHTTP(n int) *http.Client {
	short := c.shortHTTP()
	want := GroupDeliverTimeout(short.Timeout, n)
	if short.Timeout <= 0 || want <= short.Timeout {
		return short
	}
	cl := *short
	cl.Timeout = want
	return &cl
}
