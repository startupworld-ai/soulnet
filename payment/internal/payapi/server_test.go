package payapi

import (
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/startupworld-ai/soulnet/payment/internal/store"
)

func TestDecimalRoundTrip(t *testing.T) {
	cases := []struct{ in string }{
		{"1.00"}, {"0.001"}, {"1000000"}, {"0.000001"}, {"12.345678"}, {"0"},
	}
	for _, c := range cases {
		atomic, err := decimalToAtomic(c.in, 6)
		if err != nil {
			t.Fatalf("decimalToAtomic(%q): %v", c.in, err)
		}
		back := atomicToDecimal(atomic, 6)
		// "1.00" renders as "1.0" (trailing zeros trimmed); compare numerically.
		atomic2, err := decimalToAtomic(back, 6)
		if err != nil {
			t.Fatalf("decimalToAtomic(%q): %v", back, err)
		}
		if atomic2.Cmp(atomic) != 0 {
			t.Fatalf("round trip %q → %q", c.in, back)
		}
	}
}

func TestDecimalToAtomicErrors(t *testing.T) {
	for _, in := range []string{"", "abc", "1.0000001", "-", "1..2"} {
		if _, err := decimalToAtomic(in, 6); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestAtomicToDecimal(t *testing.T) {
	if got := atomicToDecimal(big.NewInt(1_000_000), 6); got != "1.0" {
		t.Fatalf("1e6 → %q", got)
	}
	if got := atomicToDecimal(big.NewInt(1), 6); got != "0.000001" {
		t.Fatalf("1 → %q", got)
	}
	if got := atomicToDecimal(new(big.Int).Neg(big.NewInt(5_000_000)), 6); got != "-5.0" {
		t.Fatalf("-5e6 → %q", got)
	}
}

func TestAccountName(t *testing.T) {
	// fingerprint base64url may contain '_' and '=' which are invalid in CDP
	// account names; they must be dropped and the result must match the
	// ^[A-Za-z0-9][A-Za-z0-9-]{0,34}[A-Za-z0-9]$ pattern.
	for _, fp := range []string{
		"aGVsbG9fd29ybGQ",
		"abcd-efgh-1234",
		"",
	} {
		name := accountName(fp)
		if len(name) < 2 || len(name) > 36 {
			t.Fatalf("accountName(%q) = %q length %d out of bounds", fp, name, len(name))
		}
		for _, ch := range name {
			ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-'
			if !ok {
				t.Fatalf("accountName(%q) = %q contains invalid char %q", fp, name, ch)
			}
		}
	}
}

func TestIsHexAddress(t *testing.T) {
	good := []string{
		"0x2e0c37b721124e2558baf75f6f8e6cc9f14aec29",
		"0x036CbD53842c5426634e7929541eC2318f3dCF7e",
	}
	for _, a := range good {
		if !isHexAddress(a) {
			t.Fatalf("%q should be a valid address", a)
		}
	}
	bad := []string{"0x1234", "0xZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", "", "0x2e0c37b721124e2558baf75f6f8e6cc9f14aec2"}
	for _, a := range bad {
		if isHexAddress(a) {
			t.Fatalf("%q should be invalid", a)
		}
	}
}

func TestWalletBindAddress(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, err := New(st, &store.Config{Mode: "local-cdp", Network: "base-sepolia"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest("POST", "/v2/pay/wallet.bind", strings.NewReader(`{"address":"0xD5D21E129B422491cfF103bA875c60dabec02899"}`))
	w := httptest.NewRecorder()
	svc.walletBind(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if out["ok"] != true || out["bound"] != "address" {
		t.Fatalf("unexpected: %v", out)
	}
	// wallet persisted under the caller fingerprint
	wal, err := st.GetWallet(fpOf(req))
	if err != nil {
		t.Fatalf("GetWallet: %v", err)
	}
	if wal == nil || wal.Address != "0xd5d21e129b422491cff103ba875c60dabec02899" {
		t.Fatalf("wallet not bound: %+v", wal)
	}
	// second bind is refused (already bound)
	w2 := httptest.NewRecorder()
	svc.walletBind(w2, httptest.NewRequest("POST", "/v2/pay/wallet.bind", strings.NewReader(`{"address":"0x1111111111111111111111111111111111111111"}`)))
	if w2.Code != 200 || !strings.Contains(w2.Body.String(), "already bound") {
		t.Fatalf("second bind should refuse: %d %s", w2.Code, w2.Body.String())
	}
}

func TestWalletBindValidation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, err := New(st, &store.Config{Mode: "local-cdp", Network: "base-sepolia"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// neither address nor account_name
	w := httptest.NewRecorder()
	svc.walletBind(w, httptest.NewRequest("POST", "/v2/pay/wallet.bind", strings.NewReader(`{}`)))
	if w.Code != ErrBadRequest && !strings.Contains(w.Body.String(), "provide exactly one") {
		t.Fatalf("expected bad request: %d %s", w.Code, w.Body.String())
	}
	// invalid address
	w2 := httptest.NewRecorder()
	svc.walletBind(w2, httptest.NewRequest("POST", "/v2/pay/wallet.bind", strings.NewReader(`{"address":"not-an-address"}`)))
	if !strings.Contains(w2.Body.String(), "provide exactly one") {
		t.Fatalf("invalid address should be refused: %s", w2.Body.String())
	}
	// CDP account_name without CDP configured
	w3 := httptest.NewRecorder()
	svc.walletBind(w3, httptest.NewRequest("POST", "/v2/pay/wallet.bind", strings.NewReader(`{"account_name":"fp-abc"}`)))
	if !strings.Contains(w3.Body.String(), "CDP not configured") {
		t.Fatalf("expected CDP not configured: %s", w3.Body.String())
	}
}
