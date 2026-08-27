// paygate is the SoulMirror payment gateway: a local standalone process that
// wraps the Coinbase CDP v2 API for USDC A2A payments.
//
// It listens on 127.0.0.1:9001 by default (loopback only), authenticates every
// request with the A2A request signature, and stores local state under
// ~/.soulmirror/a2a/pay/. CDP secrets come from the environment:
//
//	CDP_API_KEY_ID CDP_API_KEY_SECRET CDP_WALLET_SECRET   (enables local-cdp mode)
//	CDP_NETWORK=base-sepolia (default) | base
//
// Endpoints: POST /v2/pay/wallet.create, GET /v2/pay/wallet,
// POST /v2/pay/transfer, POST /v2/pay/join.verify, POST|GET /v2/pay/config,
// GET /v2/pay/health. See docs/cdp-a2a-payment-plan.md §5.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/startupworld-ai/soulnet/payment/internal/cdp"
	"github.com/startupworld-ai/soulnet/payment/internal/config"
	"github.com/startupworld-ai/soulnet/payment/internal/payapi"
	"github.com/startupworld-ai/soulnet/payment/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.HomeDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	var cdpClient *cdp.Client
	if cfg.CDPConfigured {
		cdpClient, err = cdp.NewClient(cfg.CDP)
		if err != nil {
			log.Fatalf("cdp: %v", err)
		}
	}

	cur, err := st.GetConfig()
	if err != nil {
		log.Fatalf("store config: %v", err)
	}
	cur.Network = cfg.Network

	svc, err := payapi.New(st, cur, cdpClient)
	if err != nil {
		log.Fatalf("service: %v", err)
	}
	// Pin the gateway to the local identity (when identity.json exists), so
	// other local processes cannot drive the wallet.
	if fp, err := payapi.IdentityFingerprintFromFile(cfg.IdentityFile); err != nil {
		log.Printf("warn: cannot read identity file %s (no pinning): %v", cfg.IdentityFile, err)
	} else if fp != "" {
		svc.PinIdentityFP(fp)
		log.Printf("gateway pinned to local identity fingerprint %s", fp)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("paygate listening on %s (network=%s cdp_configured=%v home=%s)",
			cfg.Listen, cfg.Network, cdpClient != nil, cfg.HomeDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	writeStateFile(cfg, cdpClient != nil)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("paygate stopped")
}

// writeStateFile records how this gateway instance was started, so support can
// read the truth off disk without logs (e.g. "<home>/gateway-state.json":
// whether CDP was configured, which network, when it started, its pid).
func writeStateFile(cfg *config.Config, cdpConfigured bool) {
	state := map[string]any{
		"ts":             time.Now().UTC().Format(time.RFC3339),
		"pid":            os.Getpid(),
		"listen":         cfg.Listen,
		"network":        cfg.Network,
		"cdp_configured": cdpConfigured,
		"identity_file":  cfg.IdentityFile,
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	tmp := cfg.HomeDir + "/gateway-state.json.tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, cfg.HomeDir+"/gateway-state.json")
}
