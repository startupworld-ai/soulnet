package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/peer"
)

// ProtocolName is the protocol identifier returned by initialize; bump it when the
// method/notification set changes.
const ProtocolName = "soulnet/1"

// JSON-RPC 2.0 standard error codes + this peer's application codes (-32000..-32099 reserved range).
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603

	codeNoIdentity     = -32001 // no identity yet (identity.create first)
	codeNotFriend      = -32002 // peer is not a friend (friends.add first) / tried to add yourself
	codeIdentityExists = -32003 // identity already exists, never overwritten
	codeNotFound       = -32004 // no such pending request / attachment not found
	codeBadCard        = -32005 // invalid card link / bad signature
	codeNetwork        = -32006 // relay / directory unreachable or returned an error
	codeBadFile        = -32007 // attachment not readable / too large / empty
	codeNoProfile      = -32008 // no capability profile yet
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Server exposes one peer.Peer as line-delimited JSON-RPC 2.0 over stdio.
type Server struct {
	n       *peer.Peer
	out     io.Writer
	outMu   sync.Mutex
	version string

	loopMu     sync.Mutex
	loopCtx    context.Context
	loopCancel context.CancelFunc
	loopDone   chan struct{}

	shutdownOnce sync.Once
	shutdown     chan struct{}
}

// NewServer creates the RPC server; events arriving via n.OnEvent are written to out as
// notifications.
func NewServer(n *peer.Peer, out io.Writer, version string) *Server {
	s := &Server{n: n, out: out, version: version, shutdown: make(chan struct{})}
	n.OnEvent = func(ev peer.Event) { s.notify(ev.Kind, ev) }
	return s
}

// StartLoop starts the receive loop in the background when an identity exists and the
// loop is not running yet (idempotent).
func (s *Server) StartLoop(parent context.Context) {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if !s.n.HasIdentity() || s.n.Running() {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.loopCtx, s.loopCancel = ctx, cancel
	done := make(chan struct{})
	s.loopDone = done
	go func() {
		defer close(done)
		if err := s.n.Run(ctx); err != nil {
			logf("receive loop exited: %v", err)
		}
	}()
}

// loopActive reports whether StartLoop has started the receive loop (without waiting for
// the goroutine to actually run, so initialize does not read false right after starting it).
func (s *Server) loopActive() bool {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	return s.loopCancel != nil
}

// StopLoop stops the receive loop and waits for it to exit.
func (s *Server) StopLoop() {
	s.loopMu.Lock()
	cancel, done := s.loopCancel, s.loopDone
	s.loopCancel, s.loopDone = nil, nil
	s.loopMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Shutdown asks the whole process to wind down (Serve returns).
func (s *Server) Shutdown() { s.shutdownOnce.Do(func() { close(s.shutdown) }) }

// Done is closed once Shutdown has been called.
func (s *Server) Done() <-chan struct{} { return s.shutdown }

// Serve reads requests line by line from in, dispatches each in its own goroutine and
// writes responses to out (correlated by id, possibly out of order — JSON-RPC 2.0
// permits that and the host matches responses by id). It returns on EOF of in, on ctx
// cancellation or on shutdown. io.EOF means the host closed stdin.
//
// Concurrent handling is deliberate: the peer's stores are lock-protected and already
// face concurrent callers (the receive loop runs beside this layer), and one slow
// request (message.send with the relay down, up to deliverTimeout × number of relays)
// must not queue every read behind it — a browser fans out a dozen reads on page open,
// and a head-of-line stall here freezes the whole UI. Notifications (incoming events)
// are unaffected: the receive loop writes them from its own goroutine.
func (s *Server) Serve(ctx context.Context, in io.Reader) error {
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			case <-s.shutdown:
				return
			}
		}
		if err := sc.Err(); err != nil {
			readErr <- err
		} else {
			readErr <- io.EOF
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.shutdown:
			return nil
		case err := <-readErr:
			return err
		case line := <-lines:
			if len(strings.TrimSpace(string(line))) == 0 {
				continue
			}
			s.handleLine(ctx, line)
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.write(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: "parse error: " + err.Error()}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.write(rpcResponse{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: &rpcError{Code: codeInvalidRequest, Message: "invalid request: need jsonrpc=2.0 and method"}})
		return
	}
	go func() {
		result, rerr := s.dispatch(ctx, req.Method, req.Params)
		if len(req.ID) == 0 || string(req.ID) == "null" {
			if req.Method == "shutdown" {
				s.Shutdown()
			}
			return // notification-style request: no response
		}
		if rerr != nil {
			s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr})
			return
		}
		if result == nil {
			result = map[string]any{"ok": true}
		}
		s.write(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
		if req.Method == "shutdown" {
			s.Shutdown() // the response is written; now wind down
		}
	}()
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (s *Server) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		logf("marshalling response failed: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(raw, '\n'))
}

func (s *Server) notify(method string, params any) {
	s.write(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
}

// ——— method dispatch ———

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	h, ok := s.methods()[method]
	if !ok {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
	res, err := h(ctx, params)
	if err != nil {
		return nil, toRPCError(err)
	}
	return res, nil
}

type handler func(ctx context.Context, params json.RawMessage) (any, error)

func (s *Server) methods() map[string]handler {
	return map[string]handler{
		"initialize":              s.initialize,
		"identity.get":            s.identityGet,
		"identity.create":         s.identityCreate,
		"card.get":                s.cardGet,
		"card.parse":              s.cardParse,
		"friends.list":            s.friendsList,
		"friends.pending":         s.friendsPending,
		"friends.add":             s.friendsAdd,
		"friends.accept":          s.friendsAccept,
		"friends.reject":          s.friendsReject,
		"friends.set":             s.friendsSet,
		"friends.card":            s.friendsCard,
		"friends.remove":          s.friendsRemove,
		"message.send":            s.messageSend,
		"message.typing":          s.messageTyping,
		"conversation.get":        s.conversationGet,
		"conversation.markRead":   s.conversationMarkRead,
		"group.create":            s.groupCreate,
		"group.list":              s.groupList,
		"group.get":               s.groupGet,
		"group.send":              s.groupSend,
		"group.conversation":      s.groupConversation,
		"group.markRead":          s.groupMarkRead,
		"group.leave":             s.groupLeave,
		"group.kick":              s.groupKick,
		"group.setProfile":        s.groupSetProfile,
		"group.pin":               s.groupPin,
		"group.unpin":             s.groupUnpin,
		"group.apply":             s.groupApply,
		"group.applications":      s.groupApplications,
		"group.approve":           s.groupApprove,
		"group.applicationReject": s.groupApplicationReject,
		"group.invite":            s.groupInvite,
		"group.voicesAnnounce":    s.groupVoicesAnnounce,
		"group.typing":            s.groupTyping,
		"artifact.path":           s.artifactPath,
		"host.relaunch":           s.hostRelaunch,
		"presence":                s.presence,
		"directory.query":         s.directoryQuery,
		"directory.fetch":         s.directoryFetch,
		"directory.publish":       s.directoryPublish,
		"directory.unpublish":     s.directoryUnpublish,
		"profile.get":             s.profileGet,
		"profile.save":            s.profileSave,
		"shutdown":                s.shutdownMethod,
	}
}

func decode(params json.RawMessage, into any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(params)))
	if err := dec.Decode(into); err != nil {
		return &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return nil
}

func invalid(msg string) error { return &rpcError{Code: codeInvalidParams, Message: msg} }

// toRPCError maps a peer error to a JSON-RPC error. The mapping relies on the peer
// package's sentinel errors (errors.Is), never on message wording.
func toRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	code := codeInternal
	switch {
	case errors.Is(err, peer.ErrNoIdentity):
		code = codeNoIdentity
	case errors.Is(err, peer.ErrNotFriend), errors.Is(err, peer.ErrSelf):
		code = codeNotFriend
	case errors.Is(err, peer.ErrNoPending), errors.Is(err, peer.ErrNoGroup):
		code = codeNotFound
	case errors.Is(err, peer.ErrGroupOwner), errors.Is(err, peer.ErrBadProfile):
		code = codeInvalidParams
	case errors.Is(err, peer.ErrBadCard):
		code = codeBadCard
	case errors.Is(err, peer.ErrNoProfile):
		code = codeNoProfile
	case errors.Is(err, peer.ErrArtifactSize), errors.Is(err, peer.ErrBadFile):
		code = codeBadFile
	case errors.Is(err, peer.ErrIdentityExists):
		code = codeIdentityExists
	case errors.Is(err, peer.ErrNetwork), strings.Contains(err.Error(), "connection"):
		code = codeNetwork
	}
	return &rpcError{Code: code, Message: err.Error()}
}

// ——— views ———

type identityView struct {
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	EdPub       string   `json:"ed_pub"`
	XPub        string   `json:"x_pub"`
	Proxies     []string `json:"proxies"`
	CreatedAt   string   `json:"created_at"`
}

func viewIdentity(id *a2a.Identity) *identityView {
	if id == nil {
		return nil
	}
	return &identityView{Name: id.Name, Fingerprint: id.Fingerprint(), EdPub: id.EdPub, XPub: id.XPub,
		Proxies: id.Proxies, CreatedAt: id.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")}
}

type cardView struct {
	URI         string    `json:"uri"`
	Fingerprint string    `json:"fingerprint"`
	Card        *a2a.Card `json:"card"`
}

func viewCard(c *a2a.Card) (*cardView, error) {
	fp, err := c.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", peer.ErrBadCard, err)
	}
	return &cardView{URI: c.EncodeURI(), Fingerprint: fp, Card: c}, nil
}

// ——— methods ———

func (s *Server) initialize(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Name) != "" && !s.n.HasIdentity() {
		if _, err := s.n.EnsureIdentity(p.Name); err != nil {
			return nil, err
		}
	}
	s.StartLoop(context.Background())
	return map[string]any{
		"protocol": ProtocolName,
		"version":  s.version,
		"home":     s.n.Home,
		"relay":    s.n.RelayBase(),
		"identity": viewIdentity(s.n.Identity()),
		"running":  s.loopActive(),
		"methods":  s.methodNames(),
		"notifications": []string{peer.EventMessageReceived, peer.EventFriendRequest, peer.EventFriendAccepted,
			peer.EventTyping, peer.EventMissionUpdate, peer.EventArtifactReady, peer.EventPresenceChanged,
			peer.EventGroupMessage, peer.EventGroupTyping, peer.EventGroupUpdated, peer.EventGroupApplication},
	}, nil
}

func (s *Server) methodNames() []string {
	m := s.methods()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) identityGet(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"identity": viewIdentity(s.n.Identity())}, nil
}

func (s *Server) identityCreate(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, invalid("name must not be empty")
	}
	id, err := s.n.CreateIdentity(p.Name)
	if err != nil {
		return nil, err
	}
	s.StartLoop(context.Background())
	return map[string]any{"identity": viewIdentity(id), "running": s.loopActive()}, nil
}

func (s *Server) cardGet(context.Context, json.RawMessage) (any, error) {
	c, err := s.n.Card()
	if err != nil {
		return nil, err
	}
	return viewCard(c)
}

func (s *Server) cardParse(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	c, err := a2a.ParseCard(p.URI)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", peer.ErrBadCard, err)
	}
	return viewCard(c)
}

func (s *Server) friendsList(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"friends": s.n.FriendList(), "pending": s.n.PendingRequests()}, nil
}

func (s *Server) friendsPending(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"pending": s.n.PendingRequests()}, nil
}

func (s *Server) friendsAdd(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		CardURI string `json:"card_uri"`
		Note    string `json:"note"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.CardURI) == "" {
		return nil, invalid("card_uri must not be empty")
	}
	fr, err := s.n.AddFriend(ctx, p.CardURI, p.Note)
	if err != nil {
		return nil, err
	}
	return map[string]any{"friend": fr}, nil
}

func (s *Server) friendsAccept(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, invalid("id must not be empty")
	}
	fr, err := s.n.Accept(ctx, p.ID, p.Note)
	if err != nil {
		return nil, err
	}
	return map[string]any{"friend": fr}, nil
}

func (s *Server) friendsReject(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, invalid("id must not be empty")
	}
	return nil, s.n.Reject(p.ID)
}

func (s *Server) friendsSet(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP       string  `json:"fp"`
		Note     string  `json:"note"`
		Protocol *string `json:"protocol"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	fr, err := s.n.SetFriend(p.FP, p.Note, p.Protocol)
	if err != nil {
		return nil, err
	}
	return map[string]any{"friend": fr}, nil
}

// friendsCard answers a friend's card link (what someone else would paste to add that
// friend) from the card snapshot kept in friends.yaml. Not a friend -> ErrNotFriend.
func (s *Server) friendsCard(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	fr := s.n.Friends.Get(p.FP)
	if fr == nil || fr.Card == nil {
		return nil, peer.ErrNotFriend
	}
	return map[string]any{"uri": fr.Card.EncodeURI(), "fingerprint": fr.Fingerprint, "card": fr.Card}, nil
}

func (s *Server) friendsRemove(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	return nil, s.n.RemoveFriend(p.FP)
}

func (s *Server) messageSend(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		To   string `json:"to"`
		Body string `json:"body"`
		File string `json:"file"`
		// Auto marks an automatic reply made by the host's alter (loop guard for the
		// receiving side; the archived entry carries it too).
		Auto bool `json:"auto"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.To == "" {
		return nil, invalid("to must not be empty")
	}
	return s.n.SendWith(ctx, p.To, p.Body, peer.SendOptions{File: p.File, Auto: p.Auto})
}

func (s *Server) messageTyping(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		To string `json:"to"`
		On *bool  `json:"on"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.To == "" {
		return nil, invalid("to must not be empty")
	}
	on := p.On == nil || *p.On
	return nil, s.n.Typing(ctx, p.To, on)
}

func (s *Server) conversationGet(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP    string `json:"fp"`
		Since int    `json:"since"`
		Limit int    `json:"limit"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	return map[string]any{"entries": s.n.Conversation(p.FP, p.Since, p.Limit), "typing": s.n.PeerTyping(p.FP)}, nil
}

func (s *Server) conversationMarkRead(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP  string `json:"fp"`
		Seq int    `json:"seq"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	return nil, s.n.MarkRead(p.FP, p.Seq)
}

func (s *Server) artifactPath(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP   string `json:"fp"`
		ID   string `json:"id"` // message id (inline attachment) or artifact_id (chunked)
		Name string `json:"name"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" || p.ID == "" || p.Name == "" {
		return nil, invalid("fp, id and name must all be non-empty")
	}
	path, err := s.n.ArtifactFile(p.FP, p.ID, p.Name)
	if err != nil {
		return nil, &rpcError{Code: codeNotFound, Message: err.Error()}
	}
	return map[string]any{"path": path}, nil
}

func (s *Server) presence(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FPs []string `json:"fps"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return map[string]any{"online": s.n.Presence(p.FPs)}, nil
}

func (s *Server) directoryQuery(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Tags  []string `json:"tags"`
		KW    string   `json:"kw"`
		Limit int      `json:"limit"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	hits, err := s.n.DirectoryQuery(p.Tags, p.KW, p.Limit)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		hits = []a2a.DirHit{}
	}
	return map[string]any{"entries": hits}, nil
}

func (s *Server) directoryFetch(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		FP string `json:"fp"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.FP == "" {
		return nil, invalid("fp must not be empty")
	}
	hit, err := s.n.DirectoryFetch(p.FP)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entry": hit}, nil
}

func (s *Server) directoryPublish(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Profile *a2a.Profile `json:"profile"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := s.n.Publish(p.Profile); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "published": true}, nil
}

func (s *Server) directoryUnpublish(context.Context, json.RawMessage) (any, error) {
	if err := s.n.Unpublish(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "published": false}, nil
}

func (s *Server) profileGet(context.Context, json.RawMessage) (any, error) {
	p, err := s.n.MyProfile()
	if err != nil {
		return nil, err
	}
	return map[string]any{"profile": p, "published": s.n.Published()}, nil
}

func (s *Server) profileSave(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Profile *a2a.Profile `json:"profile"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.Profile == nil {
		return nil, invalid("profile must not be empty")
	}
	saved, err := s.n.SaveProfile(p.Profile)
	if err != nil {
		return nil, err
	}
	return map[string]any{"profile": saved}, nil
}

func (s *Server) shutdownMethod(context.Context, json.RawMessage) (any, error) {
	// handleLine writes the response first and then triggers Shutdown (see handleLine);
	// this only answers.
	return map[string]any{"ok": true}, nil
}
