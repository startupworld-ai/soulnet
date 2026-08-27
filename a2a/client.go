package a2a

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Two HTTP timeout tiers for the relay client: long-poll (Poll; the relay waits up to
// 55s, so leave headroom) and short requests (Deliver / Ack / Presence; anything not
// answered within 15s should fail fast into the outbox instead of stalling the caller).
const (
	DefaultPollTimeout    = 70 * time.Second
	DefaultDeliverTimeout = 15 * time.Second
)

// ProxyClient is the daemon-side access layer for one relay (message broker).
// Delivery needs no auth (the envelope carries its own signature; anyone may deliver a validly
// signed letter); polling/ack must prove ownership of the mailbox.
type ProxyClient struct {
	Base string
	id   *Identity
	// HTTP serves long-poll (Poll). When ShortHTTP is nil, short requests use it too
	// (historical behaviour).
	HTTP *http.Client
	// ShortHTTP serves short requests (Deliver / Ack / Presence); nil falls back to HTTP.
	// Set it via WithDeliverTimeout. NewProxyClient leaves it nil so existing callers
	// keep their exact behaviour.
	ShortHTTP *http.Client
}

// NewProxyClient creates a client for one relay. A trailing / on base is trimmed.
// HTTP timeout is DefaultPollTimeout; short requests are not configured separately
// (ShortHTTP=nil) — chain WithDeliverTimeout when you want them.
func NewProxyClient(base string, id *Identity) *ProxyClient {
	return &ProxyClient{
		Base: strings.TrimRight(base, "/"),
		id:   id,
		HTTP: &http.Client{Timeout: DefaultPollTimeout}, // long-poll wait≤55s with headroom
	}
}

// WithDeliverTimeout gives short requests (Deliver / Ack / Presence) their own timeout
// (d<=0 means DefaultDeliverTimeout); the long-poll HTTP client is untouched. Returns c
// for chaining:
//
//	a2a.NewProxyClient(base, id).WithDeliverTimeout(0)
func (c *ProxyClient) WithDeliverTimeout(d time.Duration) *ProxyClient {
	if d <= 0 {
		d = DefaultDeliverTimeout
	}
	c.ShortHTTP = &http.Client{Timeout: d}
	return c
}

// shortHTTP returns the client for short requests (falls back to HTTP when unset).
func (c *ProxyClient) shortHTTP() *http.Client {
	if c.ShortHTTP != nil {
		return c.ShortHTTP
	}
	return c.HTTP
}

// RelayError is a non-2xx relay reply. Callers must branch on StatusCode, never
// on the message text: the public relay implementation localizes its error
// strings (the production relay answers in Chinese), and a substring match on
// English text silently never fires - the kicked-member "forget the group on
// 403" path was dead in production exactly because of that.
type RelayError struct {
	StatusCode int
	Message    string
}

func (e *RelayError) Error() string {
	return fmt.Sprintf("relay %d: %s", e.StatusCode, e.Message)
}

func apiErr(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(raw, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(raw))
	}
	return &RelayError{StatusCode: resp.StatusCode, Message: e.Error}
}

// Deliver posts an encrypted, signed outer envelope to the relay (no auth header needed).
func (c *ProxyClient) Deliver(ctx context.Context, env *Envelope) error {
	raw, _ := json.Marshal(env)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/mail", bytes.NewReader(raw))
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

// signGet adds auth headers to an inbox-type GET/POST (proves ownership of the mailbox).
func (c *ProxyClient) signGet(req *http.Request, method, path string) error {
	priv, err := c.id.EdPrivate()
	if err != nil {
		return err
	}
	ts := time.Now().Format(time.RFC3339)
	req.Header.Set(HeaderPub, c.id.EdPub)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, SignReq(priv, method, path, ts))
	return nil
}

// MailItem is one piece of pending mail at the relay (outer envelope + ack credential).
type MailItem struct {
	Envelope
	AckID string `json:"ack_id"`
}

// Poll fetches pending mail for this mailbox; waitSec>0 makes the relay long-poll.
func (c *ProxyClient) Poll(ctx context.Context, waitSec int) ([]MailItem, error) {
	path := fmt.Sprintf("/mail?box=%s&wait=%d", c.id.Fingerprint(), waitSec)
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	// The signature covers only the path (with query) — matching the relay's r.URL.RequestURI() needs care,
	// so here we always sign "/mail" (the path part); box/wait are not signed, auth only proves identity.
	if err := c.signGet(req, "GET", "/mail"); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, apiErr(resp)
	}
	var out struct {
		Messages []MailItem `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// Ack confirms receipt; the relay deletes the items.
func (c *ProxyClient) Ack(ctx context.Context, ackIDs []string) error {
	if len(ackIDs) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"box": c.id.Fingerprint(), "ack_ids": ackIDs})
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/mail/ack", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.signGet(req, "POST", "/mail/ack"); err != nil {
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

// Presence queries GET /presence?box=<fp> (unauthenticated) on this relay for each fp
// and returns fp → online (the mailbox long-polled this relay within the last 75s). The
// value mirrors the relay response {"online":bool} per fingerprint. A fingerprint whose
// query fails (network / non-200) is absent from the map and the first such error is
// returned; the others are still queried. Callers may treat "absent" as offline. Uses the
// short-request timeout (see WithDeliverTimeout).
func (c *ProxyClient) Presence(ctx context.Context, fps []string) (map[string]bool, error) {
	out := make(map[string]bool, len(fps))
	var firstErr error
	for _, fp := range fps {
		on, err := c.presenceOne(ctx, fp)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out[fp] = on
	}
	return out, firstErr
}

func (c *ProxyClient) presenceOne(ctx context.Context, fp string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/presence?box="+url.QueryEscape(fp), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.shortHTTP().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, apiErr(resp)
	}
	var out struct {
		Online bool `json:"online"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Online, nil
}

// SealEnvelope encrypts and signs an inner Message into an outer Envelope (addressed to the identity declared by toCard).
// Handshake rule (spec §4.5): when msg.Type is friend_request / friend_accept the
// envelope automatically carries from_xpub = id.XPub (the peer has no card of ours yet
// and can only derive the decryption key from it). Callers that still assign the same
// value afterwards are unaffected.
func SealEnvelope(id *Identity, toCard *Card, msg *Message) (*Envelope, error) {
	xPriv, err := id.XPrivate()
	if err != nil {
		return nil, err
	}
	xPub, err := toCard.XPublic()
	if err != nil {
		return nil, err
	}
	cipherB64, err := Seal(xPriv, xPub, msg)
	if err != nil {
		return nil, err
	}
	toFp, err := toCard.Fingerprint()
	if err != nil {
		return nil, err
	}
	edPriv, err := id.EdPrivate()
	if err != nil {
		return nil, err
	}
	ts := time.Now()
	env := &Envelope{V: 2, To: toFp, From: id.EdPub, TS: ts, Cipher: cipherB64}
	if IsHandshake(msg.Type) {
		env.FromXPub = id.XPub
	}
	env.Sig = EncodeKey(ed25519.Sign(edPriv, envelopeSigningBytes(env.To, env.TS, env.Cipher)))
	return env, nil
}

// IsHandshake reports whether msgType is a handshake message (friend_request /
// friend_accept). Their outer envelopes must carry from_xpub; a receiver can only open
// mail from a non-friend when from_xpub is present.
func IsHandshake(msgType string) bool {
	return msgType == TypeFriendRequest || msgType == TypeFriendAccept
}

// OpenFrom is the receiver-side, fully-checked variant of Open (spec §13 #1):
//  1. re-verifies the outer signature via env.VerifyEnvelope() (the relay already did,
//     but the receiver must not trust the relay);
//  2. decrypts the inner message with myX / theirX;
//  3. checks msg.From == Fingerprint(env.From) so an envelope signed by A cannot carry
//     an inner message claiming to be from B.
//
// theirX is chosen by the caller (friend card snapshot → otherwise env.FromXPub).
// Open itself is unchanged.
func OpenFrom(env *Envelope, myX *ecdh.PrivateKey, theirX *ecdh.PublicKey) (*Message, error) {
	if env == nil {
		return nil, fmt.Errorf("nil envelope")
	}
	if err := env.VerifyEnvelope(); err != nil {
		return nil, err
	}
	msg, err := Open(myX, theirX, env.Cipher)
	if err != nil {
		return nil, err
	}
	fromPub, _ := DecodeKey(env.From) // length already validated by VerifyEnvelope
	if want := Fingerprint(fromPub); msg.From != want {
		return nil, fmt.Errorf("inner from=%s does not match envelope signer %s", ShortFp(msg.From), ShortFp(want))
	}
	return msg, nil
}
