package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// The blob index of a mailbox: vault/<box>/index.jsonl on the relay's own disk, whatever
// VaultBlobStore holds the bytes. It is an append-only log of JSON lines:
//
//	{"v":1}                                   header (first line of a compacted file)
//	{"put":"<id>","size":123,"at":1700000000} blob stored (at = unix seconds)
//	{"touch":["<id>",...],"at":1700000000}    blobs confirmed (has / idempotent put): grace clock reset
//	{"del":["<id>",...]}                      blobs dropped (collection)
//
// Replaying it yields id -> (size, at), from which usage is summed. Appends are fsynced; a
// torn last line (a crash mid-append) is ignored on replay and the file is compacted. The
// log is compacted -- rewritten atomically as the header plus one put line per blob --
// once it grows past twice the live entries (plus a constant), after a collection and
// when it was rebuilt.
//
// Ordering keeps the index from ever claiming a blob the store lacks: a blob is written
// to the store before its put line, and its del line is written before the store delete.
// A crash in between leaves at worst an unindexed blob in the store (invisible, a leak),
// never an indexed one that is gone.

const vaultIndexVersion = 1

// vaultIndexCompactSlack is the constant part of the compaction threshold.
const vaultIndexCompactSlack = 1024

type vaultBlobMeta struct {
	Size int64
	At   int64 // unix seconds of the last write or confirmation
}

type vaultIndexRec struct {
	V     int      `json:"v,omitempty"`
	Put   string   `json:"put,omitempty"`
	Size  int64    `json:"size,omitempty"`
	Touch []string `json:"touch,omitempty"`
	Del   []string `json:"del,omitempty"`
	At    int64    `json:"at,omitempty"`
}

func (s *Server) vaultIndexPath(box string) string {
	return filepath.Join(s.vaultBoxDir(box), "index.jsonl")
}

// readVaultIndex replays an index file. It returns fs.ErrNotExist (wrapped) when there is
// no file, the number of lines read, and whether a torn last line was dropped.
func readVaultIndex(path string) (idx map[string]vaultBlobMeta, lines int, torn bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	idx = map[string]vaultBlobMeta{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // a touch / del line holds at most MaxVaultHasIDs ids
	endsClean := len(raw) == 0 || raw[len(raw)-1] == '\n'
	var pending []byte // the last line, parsed only once we know whether it is the tail
	flush := func(line []byte, last bool) error {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		lines++
		var rec vaultIndexRec
		if err := json.Unmarshal(line, &rec); err != nil {
			if last && !endsClean {
				torn = true
				return nil
			}
			return fmt.Errorf("vault index %s line %d is unreadable", path, lines)
		}
		if rec.V > vaultIndexVersion {
			return fmt.Errorf("vault index %s has format version %d (this relay reads %d)", path, rec.V, vaultIndexVersion)
		}
		switch {
		case rec.Put != "":
			if !a2a.ValidVaultID(rec.Put) || rec.Size <= 0 {
				return fmt.Errorf("vault index %s line %d: bad put record", path, lines)
			}
			idx[rec.Put] = vaultBlobMeta{Size: rec.Size, At: rec.At}
		case len(rec.Touch) > 0:
			for _, id := range rec.Touch {
				if m, ok := idx[id]; ok {
					m.At = rec.At
					idx[id] = m
				}
			}
		case len(rec.Del) > 0:
			for _, id := range rec.Del {
				delete(idx, id)
			}
		}
		return nil
	}
	for sc.Scan() {
		if pending != nil {
			if err := flush(pending, false); err != nil {
				return nil, 0, false, err
			}
		}
		pending = append(pending[:0:0], sc.Bytes()...)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("vault index %s: %w", path, err)
	}
	if pending != nil {
		if err := flush(pending, true); err != nil {
			return nil, 0, false, err
		}
	}
	return idx, lines, torn, nil
}

// vaultIndexAppendLocked appends recs to box's index (fsynced) and compacts it when it
// grew large. Caller holds vb.mu and has already applied recs to vb.index.
func (s *Server) vaultIndexAppendLocked(box string, vb *vaultBox, recs ...vaultIndexRec) error {
	var buf bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	path := s.vaultIndexPath(box)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("vault index append: %w", err)
	}
	vb.indexLines += len(recs)
	if vb.indexLines > 2*len(vb.index)+vaultIndexCompactSlack {
		if err := s.vaultIndexCompactLocked(box, vb); err != nil {
			return err
		}
	}
	return nil
}

// vaultIndexCompactLocked rewrites box's index atomically from vb.index. Caller holds vb.mu.
func (s *Server) vaultIndexCompactLocked(box string, vb *vaultBox) error {
	ids := make([]string, 0, len(vb.index))
	for id := range vb.index {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var buf bytes.Buffer
	head, _ := json.Marshal(vaultIndexRec{V: vaultIndexVersion})
	buf.Write(head)
	buf.WriteByte('\n')
	for _, id := range ids {
		m := vb.index[id]
		line, _ := json.Marshal(vaultIndexRec{Put: id, Size: m.Size, At: m.At})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := writeFileAtomic(s.vaultIndexPath(box), buf.Bytes()); err != nil {
		return fmt.Errorf("vault index compact: %w", err)
	}
	vb.indexLines = len(ids) + 1
	return nil
}

// vaultLoadIndexLocked fills vb.index from disk. Without an index file it rebuilds one by
// scanning the on-disk blob layout (a data directory written before the index existed) --
// but only when that layout is the configured store (or migrate is set, see
// MigrateVaultBlobs); a remote store with blobs still on local disk is an error instead
// of an index that claims blobs the store does not hold. Caller holds vb.mu.
func (s *Server) vaultLoadIndexLocked(box string, vb *vaultBox, migrate bool) error {
	onDisk := s.vaultStoreIsDisk()
	if !onDisk && !migrate && s.vaultDisk.hasBlobsDir(box) {
		return errors.New("vault: blobs of this mailbox are still on the relay's local disk but the vault store is elsewhere (MigrateVaultBlobs has not finished)")
	}
	idx, lines, torn, err := readVaultIndex(s.vaultIndexPath(box))
	switch {
	case err == nil:
		vb.index, vb.indexLines = idx, lines
		if torn {
			return s.vaultIndexCompactLocked(box, vb)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	vb.index, vb.indexLines = map[string]vaultBlobMeta{}, 0
	if !(onDisk || migrate) || !s.vaultDisk.hasBlobsDir(box) {
		return nil // no vault yet: nothing to write (read-only calls create no files)
	}
	err = s.vaultDisk.walk(box, func(id string, size int64, mod time.Time) error {
		if size > 0 {
			vb.index[id] = vaultBlobMeta{Size: size, At: mod.Unix()}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("vault: rebuild index from disk: %w", err)
	}
	return s.vaultIndexCompactLocked(box, vb)
}

// recount sums vb.index into the usage counters. Caller holds vb.mu.
func (vb *vaultBox) recount() {
	vb.bytes, vb.blobs = 0, 0
	for _, m := range vb.index {
		vb.bytes += m.Size
		vb.blobs++
	}
}
