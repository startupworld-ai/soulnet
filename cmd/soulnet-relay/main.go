// soulnet-relay -- the minimal A2A mail relay (A2A wire spec §7 mailbox + §8 capability directory).
//
//	./soulnet-relay -addr :9190 -data /var/lib/soulnet-relay [-admin-token SECRET] [-vault-quota 10G]
//
// This binary is the core relay only: store-and-forward of ciphertext envelopes, presence, health, device
// sessions, the pairing rendezvous, the per-mailbox encrypted vault and the opt-in capability directory.
// Products that mount extra services on the same listener (tunnels, app markets, ledgers, feedback
// boards ...) build their own binary around package relay's extension API; see relay/ext.go.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/startupworld-ai/soulnet/relay"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", ":9190", "listen address")
	data := flag.String("data", filepath.Join(home, ".soulnet-relay"), "data directory")
	adminToken := flag.String("admin-token", os.Getenv("SOULNET_RELAY_ADMIN_TOKEN"), "admin token handed to extensions via relay.AdminOK (the core itself has no admin routes; empty = admin checks always fail)")
	vaultQuota := flag.String("vault-quota", "10G", "per-mailbox vault quota (binary units: 10G = 10 GiB, 500M, or a byte count)")
	flag.Parse()
	quota, err := relay.ParseByteSize(*vaultQuota)
	if err != nil {
		log.Fatalf("-vault-quota: %v", err)
	}

	s, err := relay.New(*data)
	if err != nil {
		log.Fatalf("init relay storage: %v", err)
	}
	s.SetVaultQuota(quota)
	if *adminToken != "" {
		s.SetAdminToken(*adminToken)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No global WriteTimeout -- GET /mail long-polls must be able to hang for 55s.
	}
	// Device presence is written lazily; flush it when asked to stop.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		s.Flush()
		log.Printf("soulnet-relay stopping (state flushed)")
		os.Exit(0)
	}()
	log.Printf("soulnet-relay started: addr=%s data=%s vault-quota=%d bytes", *addr, *data, quota)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
