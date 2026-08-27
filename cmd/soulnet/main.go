// soulnet: the soulnet light peer.
//
// Exposes the peer package (identity / card / friends / send+receive / conversations /
// presence / directory) as line-delimited JSON-RPC 2.0 on stdin/stdout, so a host such as
// the DeepSeek Harness plugin can spawn and drive it; with --service it keeps receiving as
// a system service (it does not exit when stdin closes). stdout carries protocol frames
// only; all diagnostics go to stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/startupworld-ai/soulnet/peer"
)

// Version is injected at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func logf(format string, args ...any) { log.Printf(format, args...) }

func defaultHome() string {
	if h := os.Getenv("SOULNET_HOME"); h != "" {
		return h
	}
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		return ".soulnet"
	}
	return filepath.Join(dir, ".soulnet")
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[soulnet] ")

	// Subcommand dispatch before flag parsing: `soulnet relaunch ...` is the
	// detached restart helper of the dsh plugin's self-upgrade (relaunch.go).
	if len(os.Args) > 1 && os.Args[1] == "relaunch" {
		os.Exit(relaunchMain(os.Args[2:]))
	}

	var (
		home    = flag.String("home", defaultHome(), "data directory (default $SOULNET_HOME, then ~/.soulnet)")
		relay   = flag.String("relay", peer.DefaultRelay, "relay URL (written into identity.json when the identity is created; an existing identity uses its own proxies)")
		name    = flag.String("name", "", "create an identity with this name when none exists (empty: wait for the host to call identity.create)")
		service = flag.Bool("service", false, "service mode: keep receiving after stdin closes, until SIGINT/SIGTERM")
		showVer = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("soulnet", Version)
		return
	}

	n, err := peer.Init(*home, *relay)
	if err != nil {
		logf("init failed: %v", err)
		os.Exit(2)
	}
	// One peer per home, enforced by an OS lock: a second peer polling the
	// relay under the same identity steals mail and forks group keys. A live
	// holder = fail fast (the host's restart backoff retries until the old
	// peer is gone); a stale lock (holder died) is taken over.
	lock, err := acquirePeerLock(n.Home)
	if err != nil {
		logf("%v", err)
		os.Exit(3)
	}
	defer lock.Release()
	n.Logf = logf
	if *name != "" && !n.HasIdentity() {
		if _, err := n.EnsureIdentity(*name); err != nil {
			logf("creating identity failed: %v", err)
			os.Exit(2)
		}
	}
	if id := n.Identity(); id != nil {
		logf("home=%s relay=%s fingerprint=%s name=%s", n.Home, n.RelayBase(), id.Fingerprint(), id.Name)
	} else {
		logf("home=%s relay=%s no identity yet (waiting for identity.create)", n.Home, n.RelayBase())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := NewServer(n, os.Stdout, Version)
	srv.StartLoop(ctx) // with an identity, start receiving right away (service mode does not depend on the host's initialize)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, os.Stdin) }()

	select {
	case <-ctx.Done():
		logf("exit signal received")
	case err := <-serveErr:
		switch {
		case err == nil:
			logf("shutdown received")
		case errors.Is(err, io.EOF):
			if *service {
				logf("stdin closed; service mode keeps receiving")
				<-ctx.Done()
			} else {
				logf("stdin closed (host exited), shutting down")
			}
		default:
			logf("stdin read error: %v", err)
		}
	}
	srv.StopLoop()
}
