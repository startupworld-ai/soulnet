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
func (c *ProxyClient) DeliverGroup(ctx context.Context, env *Envelope) error {
	raw, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/group/mail", bytes.NewReader(raw))
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
