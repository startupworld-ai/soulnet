// faucet requests testnet funds from the CDP faucet (Base Sepolia).
//
// Usage:
//
//	faucet <address> <token> [count]
//
// token: usdc | eth | eurc | cbbtc (default usdc); count: number of requests
// (default 1). The CDP faucet hands out 1 USDC per request with a rolling
// 24-hour cap of 10 per address — exceeding it returns faucet_limit_exceeded
// and the tool stops. Secrets come from the environment (CDP_API_KEY_ID /
// CDP_API_KEY_SECRET / CDP_WALLET_SECRET), never from arguments.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/startupworld-ai/soulnet/payment/internal/cdp"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: faucet <address> <token> [count]")
		os.Exit(2)
	}
	address := os.Args[1]
	token := os.Args[2]
	count := 1
	if len(os.Args) >= 4 {
		if _, err := fmt.Sscanf(os.Args[3], "%d", &count); err != nil || count < 1 || count > 50 {
			fmt.Fprintln(os.Stderr, "count must be 1..50")
			os.Exit(2)
		}
	}
	network := os.Getenv("CDP_NETWORK")
	if network == "" {
		network = cdp.NetworkBaseSepolia
	}

	client, err := cdp.NewClient(cdp.Credentials{
		APIKeyID:     os.Getenv("CDP_API_KEY_ID"),
		APIKeySecret: os.Getenv("CDP_API_KEY_SECRET"),
		WalletSecret: os.Getenv("CDP_WALLET_SECRET"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cdp:", err)
		os.Exit(1)
	}

	granted := 0
	for i := 0; i < count; i++ {
		hash, err := client.RequestFaucet(network, address, token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "request %d failed: %v\n", i+1, err)
			break
		}
		granted++
		fmt.Printf("✓ %s faucet request %d → tx %s\n", token, i+1, hash)
		if i < count-1 {
			time.Sleep(3 * time.Second)
		}
	}
	fmt.Printf("granted %d of %d requested (%s on %s)\n", granted, count, token, network)
	if granted == 0 {
		os.Exit(1)
	}
}
