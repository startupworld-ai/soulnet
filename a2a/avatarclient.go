// Avatar wire client: the owner uploads / removes its picture on its own post office; anyone
// fetches a mailbox's picture from the post office named on that mailbox's card.
package a2a

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxAvatarBytes mirrors the post office's limit (relay.MaxAvatarBytes).
const MaxAvatarBytes = 256 << 10

// PutAvatar uploads our mailbox's avatar (POST /box/avatar, owner-signed). contentType is one of
// image/png, image/jpeg, image/webp, image/svg+xml.
func (c *ProxyClient) PutAvatar(ctx context.Context, data []byte, contentType string) error {
	if c.id == nil {
		return fmt.Errorf("PutAvatar: no identity")
	}
	if len(data) == 0 || len(data) > MaxAvatarBytes {
		return fmt.Errorf("PutAvatar: avatar must be 1..%d bytes", MaxAvatarBytes)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/box/avatar?box="+url.QueryEscape(c.id.Fingerprint()), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if err := c.signGet(req, "POST", "/box/avatar"); err != nil {
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

// DeleteAvatar removes our mailbox's avatar (DELETE /box/avatar, owner-signed).
func (c *ProxyClient) DeleteAvatar(ctx context.Context) error {
	if c.id == nil {
		return fmt.Errorf("DeleteAvatar: no identity")
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.Base+"/box/avatar?box="+url.QueryEscape(c.id.Fingerprint()), nil)
	if err != nil {
		return err
	}
	if err := c.signGet(req, "DELETE", "/box/avatar"); err != nil {
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

// ErrNoAvatar is returned by FetchAvatar when the mailbox has no avatar.
var ErrNoAvatar = fmt.Errorf("no avatar")

// FetchAvatar reads fp's avatar from the post office at base (GET /avatar, public). etag is the
// value from a previous fetch ("" for none); notModified reports a 304 (the cached copy is current).
func FetchAvatar(ctx context.Context, base, fp, etag string) (data []byte, contentType, newETag string, notModified bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/avatar?box="+url.QueryEscape(fp), nil)
	if err != nil {
		return nil, "", "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", "", false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, "", etag, true, nil
	case http.StatusNotFound:
		return nil, "", "", false, ErrNoAvatar
	case http.StatusOK:
	default:
		return nil, "", "", false, apiErr(resp)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, MaxAvatarBytes+1))
	if err != nil {
		return nil, "", "", false, err
	}
	if len(data) > MaxAvatarBytes {
		return nil, "", "", false, fmt.Errorf("avatar too large")
	}
	return data, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), false, nil
}
