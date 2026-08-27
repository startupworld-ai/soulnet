// The `soulnet relaunch` helper: the mechanical half of the dsh plugin's
// one-click self-upgrade.
//
// The host (dsh) cannot restart itself, and the peer must not outlive the old
// host — a second peer polling the relay under the same identity steals mail
// and forks group keys. So the peer spawns this DETACHED helper and answers
// ok; the host then exits; the helper:
//
//  1. waits for the host pid to exit (bounded; force-kills it on timeout),
//  2. waits for the OLD PEER pid to exit too (it normally follows stdin EOF;
//     bounded grace, then force-kill) — only when BOTH are provably gone
//  3. starts the host command again, detached, and exits.
//
// Every step is appended to a log file (default <home>/a2a/logs/relaunch.log)
// because during the restart window this helper is the only witness.
//
// The command line is exec'd directly (argv passed through verbatim, no shell),
// and the helper can only be spawned through the peer's stdin JSON-RPC — which
// only the host holds.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type relaunchOptions struct {
	waitPID  int           // the old host process (dsh)
	peerPID  int           // the old soulnet peer (0 = none to wait for)
	cwd      string        // working directory for the restarted command
	logPath  string        // step-by-step log file ("" = stderr only)
	command  string        // executable to restart (the host's own execPath)
	argv     []string      // its arguments, verbatim
	hostWait time.Duration // how long the host may take to exit before being killed
	peerWait time.Duration // grace for the peer after the host is gone
	poll     time.Duration
}

// relaunchMain parses `soulnet relaunch` flags and runs the sequence.
// Usage: soulnet relaunch --wait-pid N [--wait-peer N] [--cwd DIR] [--log FILE] -- <exec> [args...]
func relaunchMain(args []string) int {
	fs := flag.NewFlagSet("relaunch", flag.ContinueOnError)
	var o relaunchOptions
	fs.IntVar(&o.waitPID, "wait-pid", 0, "host pid to wait for (required)")
	fs.IntVar(&o.peerPID, "wait-peer", 0, "old peer pid that must also be gone before restarting (0 = skip)")
	fs.StringVar(&o.cwd, "cwd", "", "working directory for the restarted command")
	fs.StringVar(&o.logPath, "log", "", "append step log to this file")
	fs.DurationVar(&o.hostWait, "host-wait", 30*time.Second, "max wait for the host to exit before killing it")
	fs.DurationVar(&o.peerWait, "peer-wait", 10*time.Second, "grace for the old peer after the host is gone")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if o.waitPID <= 0 || len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: soulnet relaunch --wait-pid N [--wait-peer N] [--cwd DIR] [--log FILE] -- <exec> [args...]")
		return 2
	}
	o.command, o.argv = rest[0], rest[1:]
	o.poll = 200 * time.Millisecond
	if err := runRelaunch(o); err != nil {
		fmt.Fprintln(os.Stderr, "soulnet relaunch:", err)
		return 1
	}
	return 0
}

// runRelaunch executes the wait-host → wait/kill-peer → restart sequence.
func runRelaunch(o relaunchOptions) error {
	logw := io.Writer(os.Stderr)
	if o.logPath != "" {
		_ = os.MkdirAll(filepath.Dir(o.logPath), 0o755)
		if f, err := os.OpenFile(o.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			defer f.Close()
			logw = io.MultiWriter(os.Stderr, f)
		}
	}
	lg := log.New(logw, "", log.LstdFlags)
	lg.Printf("relaunch: begin host=%d peer=%d cmd=%q argv=%q cwd=%q", o.waitPID, o.peerPID, o.command, o.argv, o.cwd)

	// 1. The old host must be gone (it exits itself right after answering the
	// upgrade request; the timeout kill is the bound, not the plan).
	if !waitGone(o.waitPID, o.hostWait, o.poll) {
		lg.Printf("relaunch: host %d still alive after %s — killing it", o.waitPID, o.hostWait)
		if err := killProcess(o.waitPID); err != nil {
			lg.Printf("relaunch: kill host %d: %v", o.waitPID, err)
		}
		if !waitGone(o.waitPID, 10*time.Second, o.poll) {
			lg.Printf("relaunch: host %d survived the kill — aborting (must not start a second instance)", o.waitPID)
			return fmt.Errorf("host pid %d would not exit", o.waitPID)
		}
	}
	lg.Printf("relaunch: host %d is gone", o.waitPID)

	// 2. The OLD PEER must be dead before the new host may start, or two peers
	// poll the relay under one identity (mail theft + group-key forks). It
	// normally exits on stdin EOF when the host dies; the kill is the fence.
	if o.peerPID > 0 {
		if !waitGone(o.peerPID, o.peerWait, o.poll) {
			lg.Printf("relaunch: old peer %d still alive after %s — killing it", o.peerPID, o.peerWait)
			if err := killProcess(o.peerPID); err != nil {
				lg.Printf("relaunch: kill peer %d: %v", o.peerPID, err)
			}
			if !waitGone(o.peerPID, 10*time.Second, o.poll) {
				lg.Printf("relaunch: old peer %d survived the kill — aborting (a shadow peer must never coexist)", o.peerPID)
				return fmt.Errorf("old peer pid %d would not exit", o.peerPID)
			}
		}
		lg.Printf("relaunch: old peer %d is gone", o.peerPID)
	}

	// 3. Both confirmed dead — start the host command again, detached.
	cmd := exec.Command(o.command, o.argv...)
	if o.cwd != "" {
		cmd.Dir = o.cwd
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // null device
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		lg.Printf("relaunch: start %q failed: %v", o.command, err)
		return err
	}
	lg.Printf("relaunch: restarted %q pid=%d", o.command, cmd.Process.Pid)
	_ = cmd.Process.Release()
	return nil
}

// waitGone polls until pid no longer exists; false when it is still alive at the deadline.
func waitGone(pid int, limit, poll time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(poll)
	}
}

// hostRelaunch (`host.relaunch`): the dsh plugin asks for a full host restart
// after a self-upgrade. The peer spawns the detached relaunch helper and
// answers immediately; the host then exits, the peer follows on stdin EOF,
// and the helper (having confirmed both are dead) starts the command again.
// Reachable only over the peer's stdin JSON-RPC — i.e. only by the host that
// spawned this peer. argv is passed through verbatim and exec'd directly, no
// shell ever sees it.
func (s *Server) hostRelaunch(_ context.Context, params json.RawMessage) (any, error) {
	var p struct {
		PID     int      `json:"pid"`      // the host's own pid (process.pid)
		PeerPID int      `json:"peer_pid"` // this peer as the host sees it (default: our own pid)
		Exec    string   `json:"exec"`     // the host's execPath
		Argv    []string `json:"argv"`     // its arguments, verbatim
		Cwd     string   `json:"cwd"`
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if p.PID <= 0 || strings.TrimSpace(p.Exec) == "" {
		return nil, invalid("pid and exec are required")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot locate own executable: %w", err)
	}
	peerPID := p.PeerPID
	if peerPID <= 0 {
		peerPID = os.Getpid()
	}
	logPath := filepath.Join(s.n.Home, "a2a", "logs", "relaunch.log")
	args := []string{"relaunch",
		"--wait-pid", strconv.Itoa(p.PID),
		"--wait-peer", strconv.Itoa(peerPID),
		"--log", logPath,
	}
	if p.Cwd != "" {
		args = append(args, "--cwd", p.Cwd)
	}
	args = append(args, "--", p.Exec)
	args = append(args, p.Argv...)
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // null device: nothing ties it to us
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the relaunch helper failed: %w", err)
	}
	helperPID := cmd.Process.Pid
	_ = cmd.Process.Release()
	logf("host.relaunch: helper pid=%d waits host=%d peer=%d then restarts %q (log: %s)", helperPID, p.PID, peerPID, p.Exec, logPath)
	return map[string]any{"ok": true, "helper_pid": helperPID, "log": logPath}, nil
}
