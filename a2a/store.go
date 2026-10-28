package a2a

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ——— Conversation archive (A2A spec §3: conversations/<handle>/messages.jsonl) ———

// ConvEntry is one entry of the conversation archive: message + direction + delivery status.
type ConvEntry struct {
	Dir     string `json:"dir"` // "in" sent by the peer | "out" sent by us
	Message        // auto-reply flag: see Message.Auto
	// Status is meaningful only for out: sent / queued (pending resend) / error.
	Status string `json:"status,omitempty"`
	// SessionID is meaningful only for out: the action-stream session ID of the wake-up in which the local alter
	// generated this reply (= autonomy SessionRecord.ID, shaped like msg-<first 8 of peerFp>-<unixms>); the frontend
	// uses it to attach "view the processing".
	SessionID string `json:"session_id,omitempty"`
	// Recalled marks a message its sender withdrew (TypeRecall). On the receiving side
	// the body and attachment are wiped from the archive, only the shell (id, from, ts)
	// stays so clients can show "withdrawn" in its place; on the sending side the text
	// is kept (the author's own copy, e.g. to edit and resend it).
	Recalled bool `json:"recalled,omitempty"`
}

// ConvStore manages appending to and reading the conversation jsonl.
type ConvStore struct {
	dir string
	mu  sync.Mutex
	// seen is the per-peer in-memory index of "seen msgIDs" (lazily loaded). Dedup no longer scans the whole jsonl
	// per incoming message: the first access to a peer scans once to build the index, O(1) afterwards; Append maintains
	// it. Rebuilt after restart. All under mu.
	seen map[string]map[string]bool
	// lines caches the physical line count of each peer's messages.jsonl (lazily loaded,
	// maintained by AppendSeq) so AppendSeq can return the new entry's seq without
	// re-reading the file. Guarded by mu.
	lines map[string]int
}

// SeqEntry is a ConvEntry together with its 1-based physical line number ("seq") in
// conversations/<peer>/messages.jsonl. Seq is stable for the life of the file
// (append-only) and is what clients use as a read cursor / pagination key.
type SeqEntry struct {
	Seq int `json:"seq"`
	ConvEntry
}

// NewConvStore creates the conversation store.
func NewConvStore(baseDir string) *ConvStore {
	return &ConvStore{dir: filepath.Join(baseDir, "a2a", "conversations")}
}

func (s *ConvStore) file(peer string) string {
	return filepath.Join(s.dir, peer, "messages.jsonl")
}

// Append appends one conversation entry. peer is the friend fingerprint (base64url, file-name safe).
func (s *ConvStore) Append(peer string, e *ConvEntry) error {
	_, err := s.AppendSeq(peer, e)
	return err
}

// AppendSeq is Append that also returns the seq (1-based line number) the entry landed
// on, without re-reading the file. Use it when the caller must report the seq (events,
// send results): len(Recent(peer, 0)) after Append is O(n) and racy.
func (s *ConvStore) AppendSeq(peer string, e *ConvEntry) (int, error) {
	if peer == "" || strings.ContainsAny(peer, `/\`) || strings.Contains(peer, "..") {
		return 0, fmt.Errorf("非法 peer 指纹")
	}
	// AF-006 root fix on the storage side: every body entering the conversation archive (in and out alike) first has
	// internal control markers (END_OF_CONVERSATION etc.) stripped. This is the single choke point for every archiving
	// path — outbound replies, inbound mail, or the owner speaking directly — none writes internal signalling into
	// messages.jsonl, so neither the frontend nor the peer ever sees it.
	if e != nil {
		e.Body = StripControlMarkers(e.Body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.file(peer)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	// Resolve the current line count before writing so the returned seq is exact even
	// when the cache is cold.
	n, ok := s.lines[peer]
	if !ok {
		n = countLines(p)
		if s.lines == nil {
			s.lines = map[string]int{}
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return 0, err
	}
	n++
	s.lines[peer] = n
	// Maintain the seen index (if this peer's index has been built).
	if set := s.seen[peer]; set != nil {
		set[e.ID] = true
	}
	return n, nil
}

// ErrNoEntry: Update found no entry with that id in the conversation.
var ErrNoEntry = fmt.Errorf("no such message in the conversation")

// Update rewrites the one entry of the conversation with peer whose message id is id:
// fn edits it in place and reports whether anything changed (false = leave the file
// alone). Every other line is copied byte for byte and the line count is unchanged, so
// seqs stay stable. Returns the entry's seq and its (possibly edited) value, or
// ErrNoEntry. fn's error aborts the rewrite and is returned as is. Rare by design
// (one recall rewrites one file); the replacement is a temp file renamed over the old.
func (s *ConvStore) Update(peer, id string, fn func(e *ConvEntry) (bool, error)) (int, *ConvEntry, error) {
	if peer == "" || strings.ContainsAny(peer, `/\`) || strings.Contains(peer, "..") {
		return 0, nil, fmt.Errorf("非法 peer 指纹")
	}
	if id == "" {
		return 0, nil, ErrNoEntry
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.file(peer)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil, ErrNoEntry
		}
		return 0, nil, err
	}
	lines := strings.SplitAfter(string(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- { // newest first: a recall targets a recent message
		line := strings.TrimRight(lines[i], "\r\n")
		if line == "" || !strings.Contains(line, id) {
			continue
		}
		var e ConvEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.ID != id || e.Type == TypeArtifactChunk {
			continue
		}
		changed, err := fn(&e)
		if err != nil {
			return i + 1, &e, err
		}
		if !changed {
			return i + 1, &e, nil
		}
		out, err := json.Marshal(&e)
		if err != nil {
			return 0, nil, err
		}
		nl := lines[i][len(line):] // keep the original terminator ("" on an unterminated last line)
		lines[i] = string(out) + nl
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, []byte(strings.Join(lines, "")), 0o644); err != nil {
			return 0, nil, err
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return 0, nil, err
		}
		return i + 1, &e, nil
	}
	return 0, nil, ErrNoEntry
}

// countLines returns the number of physical lines in path the way bufio.Scanner counts
// them (a trailing unterminated line counts as one). Missing file → 0.
func countLines(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return 0
	}
	n := strings.Count(string(raw), "\n")
	if raw[len(raw)-1] != '\n' {
		n++
	}
	return n
}

// Since returns the entries of the conversation with peer whose seq (1-based line
// number) is > afterSeq, in file order; limit > 0 keeps only the last limit of those.
// Lines that fail to parse still occupy a seq but are not returned. Never returns nil
// (a missing conversation yields an empty slice). This is the incremental read path:
// clients remember the last seq they saw and ask for what came after instead of
// re-reading the whole file via Recent.
func (s *ConvStore) Since(peer string, afterSeq, limit int) []SeqEntry {
	out := []SeqEntry{}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.file(peer))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	seq := 0
	for sc.Scan() {
		seq++
		if seq <= afterSeq {
			continue
		}
		var e ConvEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		out = append(out, SeqEntry{Seq: seq, ConvEntry: e})
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Seen reports whether a message id is already in the conversation (idempotent dedup, spec §6).
// Uses the lazily loaded in-memory index, avoiding a full read+parse of the conversation jsonl per incoming message.
func (s *ConvStore) Seen(peer, msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.seen[peer]
	if !ok {
		set = s.loadSeenSet(peer) // first time: scan the file once to build the index (holding mu; does not call Recent to avoid re-entry)
		if s.seen == nil {
			s.seen = map[string]map[string]bool{}
		}
		s.seen[peer] = set
	}
	return set[msgID]
}

// loadSeenSet scans the conversation jsonl and builds the msgID set (caller holds s.mu).
func (s *ConvStore) loadSeenSet(peer string) map[string]bool {
	set := map[string]bool{}
	f, err := os.Open(s.file(peer))
	if err != nil {
		return set
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e ConvEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			set[e.ID] = true
		}
	}
	return set
}

// Recent returns the last n entries of the conversation with peer (n<=0 returns all), in chronological order.
func (s *ConvStore) Recent(peer string, n int) []*ConvEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.file(peer))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []*ConvEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e ConvEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, &e)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Summary scans the conversation file with peer in a single pass and returns what the friend list needs:
// total count, last entry, unread count (sent by the peer and later than since).
// Equivalent to Recent(peer, 0)+a second loop, but does not materialize the whole []*ConvEntry (the friend-list
// endpoint fully deserializing thousand-entry conversations on every page open and then discarding them is too
// wasteful). Returns (0, nil, 0) when the file cannot be read.
func (s *ConvStore) Summary(peer string, since time.Time) (count int, last *ConvEntry, unread int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.file(peer))
	if err != nil {
		return 0, nil, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var lastEntry ConvEntry
	for sc.Scan() {
		var e ConvEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		count++
		lastEntry = e
		if e.Dir == "in" && e.TS.After(since) {
			unread++
		}
	}
	if count == 0 {
		return 0, nil, 0
	}
	return count, &lastEntry, unread
}

// UnreadCount returns the number of messages in the conversation with peer that are "sent by the peer (dir=="in") and later than since".
// since is usually the friend's LastReadAt read cursor. Returns 0 when the conversation file cannot be read.
func (s *ConvStore) UnreadCount(peer string, since time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.file(peer))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e ConvEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Dir == "in" && e.TS.After(since) {
			n++
		}
	}
	return n
}

// ——— Pending friend-request queue (pending/<msg-id>.json) ———
//
// Minimal design: the only thing left in the pending queue is "friend requests" — whom to add as a friend is an
// identity-trust decision and needs the owner's nod. Conversations/jobs no longer have an approval queue; the alter
// handles them autonomously per the diplomatic protocol.

// Pending is one friend request awaiting the owner.
type Pending struct {
	ID        string    `json:"id"`   // = request message id
	Peer      string    `json:"peer"` // requester fingerprint
	Incoming  Message   `json:"incoming"`
	CreatedAt time.Time `json:"created_at"`
}

// PendingStore manages the pending draft files.
type PendingStore struct {
	dir string
	mu  sync.Mutex
}

// NewPendingStore creates the pending store.
func NewPendingStore(baseDir string) *PendingStore {
	return &PendingStore{dir: filepath.Join(baseDir, "a2a", "pending")}
}

func (s *PendingStore) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("非法草稿 id")
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Put writes one draft.
func (s *PendingStore) Put(p *Pending) error {
	fp, err := s.path(p.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(fp, raw, 0o644)
}

// Get fetches one draft; returns nil when absent.
func (s *PendingStore) Get(id string) *Pending {
	fp, err := s.path(id)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(fp)
	if err != nil {
		return nil
	}
	var p Pending
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return &p
}

// Delete removes a draft (after approve / reject).
func (s *PendingStore) Delete(id string) error {
	fp, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(fp)
}

// List returns all pending drafts, oldest first by creation time.Never returns nil (JSON "[]" when empty).
func (s *PendingStore) List() []*Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*Pending{}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var p Pending
		if json.Unmarshal(raw, &p) == nil {
			out = append(out, &p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
