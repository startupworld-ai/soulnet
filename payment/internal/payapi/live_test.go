package payapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/payment/internal/store"
)

// End-to-end smoke test against the real public Base Sepolia RPC.
// Run with: PAYGATE_LIVE=1 go test ./payment/internal/payapi -run TestLiveJoinVerify
//
// Uses the real USDC transfer tx 0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7
// (1000 atomic USDC, i.e. 0.001, paid to 0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29).
func TestLiveJoinVerify(t *testing.T) {
	if os.Getenv("PAYGATE_LIVE") == "" {
		t.Skip("set PAYGATE_LIVE=1 to run the live chain test")
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(&store.Config{Mode: "manual-address", Network: "base-sepolia"}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, &store.Config{Mode: "manual-address", Network: "base-sepolia"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// Sign an A2A request as a fresh identity.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v2/pay/join.verify"
	ts := time.Now().UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]string{
		"tx_hash": "0xa13a28cb667919dc675d6401bcd6bd2329e8e6d612e8bbbfc1bf547602eec3c7",
		"to":      "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29",
		"amount":  "0.001",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
	req.Header.Set(a2a.HeaderPub, a2a.EncodeKey(priv.Public().(ed25519.PublicKey)))
	req.Header.Set(a2a.HeaderTimestamp, ts)
	req.Header.Set(a2a.HeaderSignature, a2a.SignReq(priv, http.MethodPost, path, ts))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	t.Logf("join.verify response: %v", out)
	if out["valid"] != true {
		t.Fatalf("expected valid=true, got %v", out)
	}
}
