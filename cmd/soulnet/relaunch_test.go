package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: it is the fake host / fake restart
// target the relaunch tests spawn (standard re-exec pattern). Behavior comes
// from the argv after "--": `sleep <duration>` or `touch <file>`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SOULNET_RELAUNCH_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	switch {
	case len(args) >= 2 && args[0] == "sleep":
		d, err := time.ParseDuration(args[1])
		if err != nil {
			os.Exit(3)
		}
		time.Sleep(d)
	case len(args) >= 2 && args[0] == "touch":
		if err := os.WriteFile(args[1], []byte("ok"), 0o644); err != nil {
			os.Exit(3)
		}
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

// spawnHelper starts this test binary as a helper process running `verb args...`.
func spawnHelper(t *testing.T, verb string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestHelperProcess", "--", verb}, args...)...)
	cmd.Env = append(os.Environ(), "SOULNET_RELAUNCH_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning the fake process failed: %v", err)
	}
	// Reap the child the moment it dies. In production nothing the relaunch
	// helper waits on is its own child, so a killed process disappears for
	// kill(pid, 0) immediately; here the TEST is the parent, and without a
	// waiter the corpse stays a zombie - which kill(2) still counts as alive,
	// making the helper believe its kill failed (caught on the unix CI legs).
	go func() { _ = cmd.Wait() }()
	return cmd
}

// waitForFile polls until the file exists (the restarted target ran) or times out.
func waitForFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("restart target never ran: %s does not exist", path)
}

func relaunchTestOptions(t *testing.T, hostPID, peerPID int, marker, logPath string) relaunchOptions {
	t.Helper()
	// The restarted "host" is this test binary touching a marker file. The
	// spawned target inherits the test process env, which must carry the
	// helper gate.
	t.Setenv("SOULNET_RELAUNCH_HELPER", "1")
	return relaunchOptions{
		waitPID:  hostPID,
		peerPID:  peerPID,
		cwd:      t.TempDir(),
		logPath:  logPath,
		command:  os.Args[0],
		argv:     []string{"-test.run=TestHelperProcess", "--", "touch", marker},
		hostWait: 5 * time.Second,
		peerWait: 5 * time.Second,
		poll:     50 * time.Millisecond,
	}
}

// The normal path: host and peer exit by themselves, then the target command runs.
func TestRelaunchWaitsThenRestarts(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	logPath := filepath.Join(dir, "relaunch.log")
	host := spawnHelper(t, "sleep", "300ms")
	peer := spawnHelper(t, "sleep", "500ms")
	o := relaunchTestOptions(t, host.Process.Pid, peer.Process.Pid, marker, logPath)
	if err := runRelaunch(o); err != nil {
		t.Fatalf("runRelaunch: %v", err)
	}
	waitForFile(t, marker, 10*time.Second)
	_ = host.Wait()
	_ = peer.Wait()
	logText, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("relaunch log missing: %v", err)
	}
	for _, want := range []string{"relaunch: begin", "is gone", "relaunch: restarted"} {
		if !strings.Contains(string(logText), want) {
			t.Errorf("relaunch log lacks %q:\n%s", want, logText)
		}
	}
}

// The fence: a host that never exits is killed at the timeout, a peer that
// never exits is killed after its grace — and only then does the target run.
func TestRelaunchKillsStragglers(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	logPath := filepath.Join(dir, "relaunch.log")
	host := spawnHelper(t, "sleep", "1h")
	peer := spawnHelper(t, "sleep", "1h")
	defer func() { _ = host.Process.Kill(); _ = peer.Process.Kill() }()
	o := relaunchTestOptions(t, host.Process.Pid, peer.Process.Pid, marker, logPath)
	o.hostWait = 300 * time.Millisecond
	o.peerWait = 300 * time.Millisecond
	if err := runRelaunch(o); err != nil {
		t.Fatalf("runRelaunch: %v", err)
	}
	waitForFile(t, marker, 10*time.Second)
	_ = host.Wait() // reap; must be dead now
	_ = peer.Wait()
	if processAlive(host.Process.Pid) {
		t.Error("host still alive after runRelaunch")
	}
	if processAlive(peer.Process.Pid) {
		t.Error("old peer still alive after runRelaunch")
	}
	logText, _ := os.ReadFile(logPath)
	for _, want := range []string{"killing it", "relaunch: restarted"} {
		if !strings.Contains(string(logText), want) {
			t.Errorf("relaunch log lacks %q:\n%s", want, logText)
		}
	}
}

// processAlive tells a live process from a dead one (both platforms).
func TestProcessAlive(t *testing.T) {
	proc := spawnHelper(t, "sleep", "2s")
	if !processAlive(proc.Process.Pid) {
		t.Error("running process reported dead")
	}
	_ = proc.Process.Kill()
	_ = proc.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(proc.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(proc.Process.Pid) {
		t.Error("killed and reaped process still reported alive")
	}
}
