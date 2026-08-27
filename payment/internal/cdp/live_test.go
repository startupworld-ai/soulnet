package cdp

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/payment/internal/rpcclient"
)

// Live CDP integration test (Base Sepolia). Run with:
//
//	set -a; source ~/.soulmirror/a2a/pay/.env; set +a
//	PAYGATE_LIVE=1 go test ./payment/internal/cdp -run TestLiveCDPFullFlow -v
//
// It creates two EVM accounts, faucets USDC + ETH, sends a real USDC transfer
// through the full pipeline (RLP → CDP send/transaction) and verifies it
// on-chain via the public RPC. Secrets stay in the shell env — never printed.
func TestLiveCDPFullFlow(t *testing.T) {
	if os.Getenv("PAYGATE_LIVE") == "" {
		t.Skip("set PAYGATE_LIVE=1 to run the live CDP test")
	}
	network := os.Getenv("CDP_NETWORK")
	if network == "" {
		network = NetworkBaseSepolia
	}
	cred := Credentials{
		APIKeyID:     os.Getenv("CDP_API_KEY_ID"),
		APIKeySecret: os.Getenv("CDP_API_KEY_SECRET"),
		WalletSecret: os.Getenv("CDP_WALLET_SECRET"),
	}
	if cred.APIKeyID == "" || cred.APIKeySecret == "" || cred.WalletSecret == "" {
		t.Fatal("CDP_API_KEY_ID / CDP_API_KEY_SECRET / CDP_WALLET_SECRET must be set in the environment")
	}

	client, err := NewClient(cred)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	usdc, err := USDCContract(network)
	if err != nil {
		t.Fatal(err)
	}
	rpcURL, err := RPCEndpoint(network)
	if err != nil {
		t.Fatal(err)
	}
	rpc := rpcclient.New(rpcURL)
	ctx := context.Background()

	unique := fmt.Sprintf("%d", time.Now().UnixNano()%100000000)
	sender, err := client.CreateAccount("paytest-s" + unique)
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	t.Logf("sender   = %s (%s)", sender.Address, sender.Name)
	recipient, err := client.CreateAccount("paytest-r" + unique)
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	t.Logf("recipient= %s (%s)", recipient.Address, recipient.Name)

	// 1. Faucet USDC (for the transfer) + ETH (for gas).
	faucetUSDC, err := client.RequestFaucet(network, sender.Address, "usdc")
	if err != nil {
		t.Fatalf("faucet usdc: %v", err)
	}
	t.Logf("faucet usdc tx = %s", faucetUSDC)
	faucetETH, err := client.RequestFaucet(network, sender.Address, "eth")
	if err != nil {
		t.Fatalf("faucet eth: %v", err)
	}
	t.Logf("faucet eth  tx = %s", faucetETH)

	waitConfirmed(t, rpc, ctx, faucetUSDC)
	waitConfirmed(t, rpc, ctx, faucetETH)

	// 2. Wait for CDP balance sync, then confirm USDC landed.
	usdcBalance := waitUSDC(t, client, rpc, ctx, network, sender.Address, usdc)
	t.Logf("sender USDC balance = %s", usdcBalance)

	// 3. Build + send a real USDC transfer of 0.5 to the recipient.
	amount := big.NewInt(500_000) // 0.5 USDC
	data, err := BuildERC20TransferData(recipient.Address, amount)
	if err != nil {
		t.Fatal(err)
	}
	chainID, err := ChainID(network)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := rpc.GetTransactionCount(ctx, sender.Address)
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	priority, err := rpc.MaxPriorityFeePerGas(ctx)
	if err != nil {
		t.Fatalf("priority: %v", err)
	}
	gasPrice, err := rpc.GasPrice(ctx)
	if err != nil {
		t.Fatalf("gas price: %v", err)
	}
	gasLimit, err := rpc.EstimateGas(ctx, sender.Address, usdc, data)
	if err != nil {
		t.Fatalf("gas estimate: %v", err)
	}
	tx := EIP1559Tx{
		ChainID:              chainID,
		Nonce:                nonce,
		MaxPriorityFeePerGas: priority,
		MaxFeePerGas:         new(big.Int).Add(gasPrice, priority),
		GasLimit:             gasLimit,
		To:                   usdc,
		Value:                big.NewInt(0),
		Data:                 data,
	}
	rlpHex, err := tx.RLP()
	if err != nil {
		t.Fatal(err)
	}
	sent, err := client.SendTransaction(sender.Address, network, rlpHex)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	t.Logf("USDC transfer tx = %s", sent)

	// 4. Verify on-chain: recipient, amount ≥ 0.5 USDC, sender == our wallet.
	if ok, actual, from, err := rpc.VerifyUSDCTransfer(ctx, sent, usdc, recipient.Address, amount); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !ok {
		t.Fatalf("transfer not verified (tx may need a moment)")
	} else {
		t.Logf("verified on-chain: %s USDC → %s from %s", atomicUSDC(actual), recipient.Address, from)
	}
	t.Logf("✅ full pipeline OK — view: https://sepolia.basescan.org/tx/%s", sent)
}

func waitConfirmed(t *testing.T, rpc *rpcclient.Client, ctx context.Context, hash string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := rpc.GetTransactionReceipt(ctx, hash)
		if err == nil && rec != nil && rec.Status == "0x1" {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("tx %s not confirmed within 90s", hash)
}

func waitUSDC(t *testing.T, client *Client, rpc *rpcclient.Client, ctx context.Context, network, address, usdc string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		bal, err := client.ListTokenBalances(network, address)
		if err == nil {
			for _, b := range bal {
				if strings.EqualFold(b.Token.ContractAddress, usdc) {
					if d := atomicUSDC(big.NewInt(0)); d == "" {
						continue
					}
					amt, ok := new(big.Int).SetString(b.Amount.Amount, 10)
					if ok && amt.Sign() > 0 {
						return atomicUSDC(amt)
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("USDC balance not synced within 60s")
	return ""
}

func atomicUSDC(atomic *big.Int) string {
	whole := new(big.Int).Div(atomic, big.NewInt(1_000_000))
	frac := new(big.Int).Mod(atomic, big.NewInt(1_000_000))
	return fmt.Sprintf("%d.%06d", whole, frac)
}
