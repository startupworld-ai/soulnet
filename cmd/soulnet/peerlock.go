// Per-home single-instance lock of the peer.
//
// Two peers polling the relay under ONE identity steal each other's mail and
// fork group keys (the "shadow peer" incident class), so a home directory may
// hold at most one running peer. Different homes are independent — several
// dsh instances with their own homes on one machine are fine.
//
// The guarantee is an OPERATING-SYSTEM lock on <home>/peer.lock, not a pid
// file convention: flock(LOCK_EX|LOCK_NB) on unix, a write-share-denying open
// on Windows (per-platform openLocked in peerlock_*.go). The kernel releases
// it whatever way the holder dies, so a stale file from a crashed peer is
// taken over silently; a LIVE holder makes the second peer fail fast with the
// holder's pid in the error (the pid is written into the file, which stays
// readable to others).
//
// The `soulnet relaunch` helper never touches this lock — it is dispatched in
// main() before the peer path and does not open the home for receiving.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// errLockHeld is openLocked's sentinel: a live process holds the lock.
var errLockHeld = errors.New("peer lock held")

type peerLock struct {
	file *os.File
	path string
}

// acquirePeerLock takes the exclusive per-home lock and records our pid in it.
// A live holder yields an error naming the holding pid; a stale file (holder
// died) is taken over.
func acquirePeerLock(home string) (*peerLock, error) {
	path := filepath.Join(home, "peer.lock")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create home %s: %w", home, err)
	}
	f, err := openLocked(path)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			holder := "unknown"
			if content, readErr := os.ReadFile(path); readErr == nil {
				if pid := strings.TrimSpace(string(content)); pid != "" {
					holder = pid
				}
			}
			return nil, fmt.Errorf("another soulnet peer is already running for this home (pid %s holds %s) — a second peer on one identity steals mail and forks group keys; not starting", holder, path)
		}
		return nil, fmt.Errorf("cannot lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, 0)
		_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		_ = f.Sync()
	}
	return &peerLock{file: f, path: path}, nil
}

// Release drops the lock (closing the descriptor releases the OS lock; the
// file itself stays — a file without a live lock is stale by definition).
func (l *peerLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil
}
