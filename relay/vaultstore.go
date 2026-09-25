package relay

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
)

// VaultBlobStore holds the bytes of vault blobs (ciphertext, content-addressed by the
// client). The relay keeps everything else about the vault on its own disk -- lane heads,
// the per-mailbox blob index (id -> size, write time) and the usage counters -- so has,
// quota checks and garbage collection never ask the store what it holds. A store only
// ever sees ids the relay validated (a2a.ValidVaultID) and boxes that passed SafeBox.
//
// The default store is DiskVaultBlobStore under the data directory; a deployment can put
// blobs elsewhere (e.g. an object store) with Server.SetVaultBlobStore.
type VaultBlobStore interface {
	// Put stores data under (box, id), replacing any previous bytes. It must be atomic:
	// a concurrent or later Get sees either nothing or all of data.
	Put(ctx context.Context, box, id string, data []byte) error
	// Get returns the bytes stored under (box, id), or an error wrapping
	// ErrVaultBlobNotFound when there are none.
	Get(ctx context.Context, box, id string) ([]byte, error)
	// Delete removes (box, id). Deleting a blob that is not stored is not an error.
	Delete(ctx context.Context, box, id string) error
	// DeleteBox removes every blob of box. A box without blobs is not an error.
	DeleteBox(ctx context.Context, box string) error
}

// ErrVaultBlobNotFound is what VaultBlobStore.Get wraps when a blob is not stored.
var ErrVaultBlobNotFound = errors.New("vault blob not found")

// DiskVaultBlobStore is the default VaultBlobStore: one file per blob under
// <dataDir>/vault/<box>/blobs/<id[:2]>/<id>, uploads staged in <dataDir>/vault/<box>/tmp/
// and renamed into place. This is also the layout relays wrote before the store was
// pluggable, so an existing data directory needs no conversion.
type DiskVaultBlobStore struct {
	root string // <dataDir>/vault
}

// NewDiskVaultBlobStore returns the disk store of the relay data directory dataDir.
func NewDiskVaultBlobStore(dataDir string) *DiskVaultBlobStore {
	return &DiskVaultBlobStore{root: filepath.Join(dataDir, "vault")}
}

func (d *DiskVaultBlobStore) blobsDir(box string) string { return filepath.Join(d.root, box, "blobs") }

func (d *DiskVaultBlobStore) blobPath(box, id string) string {
	return filepath.Join(d.blobsDir(box), id[:2], id)
}

// Put writes data to a temp file, fsyncs it and renames it into place.
func (d *DiskVaultBlobStore) Put(_ context.Context, box, id string, data []byte) error {
	tmpDir := filepath.Join(d.root, box, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	dst := d.blobPath(box, id)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(dst), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// Get reads the blob file.
func (d *DiskVaultBlobStore) Get(_ context.Context, box, id string) ([]byte, error) {
	f, err := os.Open(d.blobPath(box, id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrVaultBlobNotFound
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrVaultBlobNotFound
	}
	return io.ReadAll(io.LimitReader(f, a2a.MaxVaultBlobBytes+1))
}

// Delete removes the blob file and, when that leaves its shard directory empty, the shard.
func (d *DiskVaultBlobStore) Delete(_ context.Context, box, id string) error {
	p := d.blobPath(box, id)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(p)) // only succeeds when empty
	return nil
}

// DeleteBox removes the blob and upload directories of box (its metadata is the relay's).
func (d *DiskVaultBlobStore) DeleteBox(_ context.Context, box string) error {
	if err := os.RemoveAll(d.blobsDir(box)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(d.root, box, "tmp"))
}

// hasBlobsDir reports whether box has a blob directory on disk (the legacy layout).
func (d *DiskVaultBlobStore) hasBlobsDir(box string) bool {
	info, err := os.Stat(d.blobsDir(box))
	return err == nil && info.IsDir()
}

// walk calls fn for every blob file of box, with its size and modification time. A box
// without a blob directory walks as empty.
func (d *DiskVaultBlobStore) walk(box string, fn func(id string, size int64, mod time.Time) error) error {
	return filepath.WalkDir(d.blobsDir(box), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || !a2a.ValidVaultID(e.Name()) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return fn(e.Name(), info.Size(), info.ModTime())
	})
}
