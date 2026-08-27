package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// One home, one peer: the second acquisition fails fast and names the holder.
func TestPeerLockExclusivePerHome(t *testing.T) {
	home := t.TempDir()
	first, err := acquirePeerLock(home)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	if _, err := acquirePeerLock(home); err == nil {
		t.Fatal("second acquire for the same home succeeded; want fail-fast")
	} else if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("error does not name the holding pid: %v", err)
	}
	first.Release()
	third, err := acquirePeerLock(home)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	third.Release()
}

// Different homes are independent — multiple dsh instances on one machine.
func TestPeerLockDifferentHomes(t *testing.T) {
	a, err := acquirePeerLock(t.TempDir())
	if err != nil {
		t.Fatalf("home A: %v", err)
	}
	defer a.Release()
	b, err := acquirePeerLock(t.TempDir())
	if err != nil {
		t.Fatalf("home B: %v", err)
	}
	b.Release()
}

// A lock FILE without a live holder (crashed peer) is taken over.
func TestPeerLockStaleTakeover(t *testing.T) {
	home := t.TempDir()
	stale := filepath.Join(home, "peer.lock")
	if err := os.WriteFile(stale, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := acquirePeerLock(home)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	defer l.Release()
	content, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file pid = %q, want our pid %d", strings.TrimSpace(string(content)), os.Getpid())
	}
}
