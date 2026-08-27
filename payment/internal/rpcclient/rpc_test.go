package rpcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real USDC Transfer on Base Sepolia (fetched from sepolia.base.org):
// tx 0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7
// sent 1000 atomic (0.001 USDC) from 0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29 to itself.
const realFrom = "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29"

const realReceiptJSON = `{
  "transactionHash": "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7",
  "status": "0x1",
  "blockNumber": "0x2bda63f",
  "from": "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29",
  "logs": [
    {
      "address": "0x036cbd53842c5426634e7929541ec2318f3dcf7e",
      "topics": [
        "0x98de503528ee59b575ef0c0a2576a82497bfc029a5685b209e9ec333479b10a5",
        "0x0000000000000000000000002e0c37b721124e2558baf75f6f8e6cc9f14aec29",
        "0xf34c848752a04dd68d91b457ee40349e0be5802654b2d8778c829fc776724646"
      ],
      "data": "0x",
      "logIndex": "0x1a"
    },
    {
      "address": "0x036cbd53842c5426634e7929541ec2318f3dcf7e",
      "topics": [
        "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
        "0x0000000000000000000000002e0c37b721124e2558baf75f6f8e6cc9f14aec29",
        "0x0000000000000000000000002e0c37b721124e2558baf75f6f8e6cc9f14aec29"
      ],
      "data": "0x00000000000000000000000000000000000000000000000000000000000003e8",
      "logIndex": "0x1b"
    }
  ]
}`

const (
	realTxHash  = "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7"
	realTo      = "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29"
	usdcSepolia = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
)

// newStubClient serves receiptJSON for eth_getTransactionReceipt via a real
// HTTP server, exercising the actual JSON-RPC path.
func newStubClient(t *testing.T, receiptJSON string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if receiptJSON == "" {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":null}`)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%s}`, receiptJSON)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestVerifyUSDCTransferRealVector(t *testing.T) {
	c := newStubClient(t, realReceiptJSON)
	ctx := context.Background()

	// 1) matching recipient, amount >= min → valid, sender reported
	ok, amt, from, err := c.VerifyUSDCTransfer(ctx, realTxHash, usdcSepolia, realTo, big.NewInt(1000))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || amt.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("expected valid with 1000, got ok=%v amt=%v", ok, amt)
	}
	if !strings.EqualFold(from, realFrom) {
		t.Fatalf("sender should be %s, got %s", realFrom, from)
	}
	// 2) amount below min → invalid, amount still reported
	ok, amt, _, err = c.VerifyUSDCTransfer(ctx, realTxHash, usdcSepolia, realTo, big.NewInt(1001))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected invalid when amount below minimum")
	}
	if amt == nil || amt.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("amount should be reported as 1000, got %v", amt)
	}
	// 3) wrong recipient → invalid
	ok, _, _, err = c.VerifyUSDCTransfer(ctx, realTxHash, usdcSepolia, "0x0000000000000000000000000000000000000001", big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected invalid for wrong recipient")
	}
}

func TestVerifyUSDCTransferEdgeCases(t *testing.T) {
	// reverted tx
	var rec struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal([]byte(realReceiptJSON), &rec)
	reverted := `{"transactionHash":"` + realTxHash + `","status":"0x0","logs":[]}`
	if ok, _, _, err := newStubClient(t, reverted).VerifyUSDCTransfer(context.Background(), realTxHash, usdcSepolia, realTo, big.NewInt(1)); err != nil || ok {
		t.Fatalf("reverted: ok=%v err=%v", ok, err)
	}

	// tx not found → verified=false, err=nil (the "not yet" case)
	if ok, _, _, err := newStubClient(t, "").VerifyUSDCTransfer(context.Background(), realTxHash, usdcSepolia, realTo, big.NewInt(1)); err != nil || ok {
		t.Fatalf("nil receipt: ok=%v err=%v", ok, err)
	}

	// Transfer log for a different contract is ignored
	other := `{"transactionHash":"` + realTxHash + `","status":"0x1","logs":[{"address":"0x1111111111111111111111111111111111111111","topics":["` + TransferTopic + `","0x0000000000000000000000002e0c37b721124e2558baf75f6f8e6cc9f14aec29","0x0000000000000000000000002e0c37b721124e2558baf75f6f8e6cc9f14aec29"],"data":"0x00000000000000000000000000000000000000000000000000000000000003e8"}]}`
	if ok, _, _, err := newStubClient(t, other).VerifyUSDCTransfer(context.Background(), realTxHash, usdcSepolia, realTo, big.NewInt(1)); err != nil || ok {
		t.Fatalf("wrong contract: ok=%v err=%v", ok, err)
	}
}

func TestHexToBig(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0x0", 0}, {"0", 0}, {"0x1", 1}, {"0x1a", 26}, {"0x2bda63f", 45983295},
	}
	for _, c := range cases {
		v, err := hexToBig(c.in)
		if err != nil {
			t.Fatalf("hexToBig(%q): %v", c.in, err)
		}
		if v.Int64() != c.want {
			t.Fatalf("hexToBig(%q) = %d, want %d", c.in, v.Int64(), c.want)
		}
	}
	if _, err := hexToBig("zz"); err == nil {
		t.Fatal("expected error for invalid hex")
	}
}
