// Package payapi exposes the gateway's HTTP surface (/v2/pay/*) on a loopback
// listener. Every request is authenticated with the A2A request signature
// (X-A2A-Pub / X-A2A-Timestamp / X-A2A-Signature, same format as the relay's
// VerifyRequest) so the caller's fingerprint is authoritative — the gateway
// never trusts an ambient "who am I" field.
package payapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/startupworld-ai/soulnet/a2a"
	"github.com/startupworld-ai/soulnet/payment/internal/cdp"
	"github.com/startupworld-ai/soulnet/payment/internal/rpcclient"
	"github.com/startupworld-ai/soulnet/payment/internal/store"
)

// Error codes shared with the peer JSON-RPC surface.
const (
	ErrCDPNotConfigured = -32010 // action needs CDP but the user has not configured it
	ErrNoWallet         = -32011 // wallet not created yet for this fingerprint
	ErrBadRequest       = -32602
	ErrUnauthorized     = -32600
)

// Service wires config + store + CDP client + RPC client behind the HTTP mux.
type Service struct {
	store   *store.Store
	cfg     *configHolder
	cdp     *cdp.Client
	rpc     *rpcclient.Client
	network string
	// pinFP is the only fingerprint allowed to call the gateway. Empty =
	// standalone/dev mode (accepts any valid A2A signature); when the gateway
	// is spawned by the plugin it is set from the local identity.json so that
	// other local processes cannot drive the wallet.
	pinFP string
}

type configHolder struct{ c *store.Config }

// New builds the service. cfgCDP may be nil when CDP is not configured yet
// (mode "manual-address"); the endpoints that need CDP return ErrCDPNotConfigured.
func New(st *store.Store, c *store.Config, cdpClient *cdp.Client) (*Service, error) {
	rpcURL, err := cdp.RPCEndpoint(c.Network)
	if err != nil {
		return nil, err
	}
	return &Service{
		store:   st,
		cfg:     &configHolder{c: c},
		cdp:     cdpClient,
		rpc:     rpcclient.New(rpcURL),
		network: c.Network,
	}, nil
}

// PinIdentityFP restricts callers to one fingerprint (from the local
// identity.json). See the pinFP field.
func (s *Service) PinIdentityFP(fp string) { s.pinFP = fp }

// Handler returns the HTTP mux with all /v2/pay/* routes behind A2A verification.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/pay/wallet.create", s.requireAuth(s.walletCreate))
	mux.HandleFunc("POST /v2/pay/wallet.bind", s.requireAuth(s.walletBind))
	mux.HandleFunc("GET /v2/pay/wallet", s.requireAuth(s.walletBalance))
	mux.HandleFunc("POST /v2/pay/transfer", s.requireAuth(s.transfer))
	mux.HandleFunc("POST /v2/pay/join.verify", s.requireAuth(s.joinVerify))
	mux.HandleFunc("POST /v2/pay/join.receipt", s.requireAuth(s.joinReceipt))
	mux.HandleFunc("POST /v2/pay/config", s.requireAuth(s.setConfig))
	mux.HandleFunc("GET /v2/pay/config", s.requireAuth(s.getConfig))
	mux.HandleFunc("GET /v2/pay/health", s.health)
	return mux
}

// requireAuth verifies the A2A request signature and injects the caller's
// fingerprint into the context. When pinFP is set, only that fingerprint may
// call the gateway (defense against other local processes).
func (s *Service) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fp, err := a2a.VerifyReq(
			r.Header.Get(a2a.HeaderPub),
			r.Method, r.URL.Path,
			r.Header.Get(a2a.HeaderTimestamp),
			r.Header.Get(a2a.HeaderSignature),
		)
		if err != nil {
			writeError(w, ErrUnauthorized, "a2a signature: "+err.Error())
			return
		}
		if s.pinFP != "" && fp != s.pinFP {
			writeError(w, ErrUnauthorized, "signer is not the local identity")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyFP{}, fp)
		next(w, r.WithContext(ctx))
	}
}

// IdentityFingerprintFromFile reads the fingerprint of the local identity
// (identity.json, ed_pub field). Returns "" when the file is absent.
func IdentityFingerprintFromFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var id struct {
		EdPub string `json:"ed_pub"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", fmt.Errorf("identity.json: %w", err)
	}
	pub, err := a2a.DecodeKey(id.EdPub)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("identity.json: bad ed_pub")
	}
	return a2a.Fingerprint(pub), nil
}

type ctxKeyFP struct{}

func fpOf(r *http.Request) string {
	fp, _ := r.Context().Value(ctxKeyFP{}).(string)
	return fp
}

// ——— handlers ———

// walletCreate is get-or-create: bind the caller's fingerprint to a CDP EVM
// account in the user's own project (account name derived from the fingerprint,
// unique per project).
func (s *Service) walletCreate(w http.ResponseWriter, r *http.Request) {
	fp := fpOf(r)
	if s.cdp == nil {
		writeError(w, ErrCDPNotConfigured, "CDP not configured: 设置 → 灵镜网络 → CDP")
		return
	}
	existing, err := s.store.GetWallet(fp)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	if existing != nil {
		writeJSON(w, map[string]any{
			"address": existing.Address, "network": existing.Network, "created": false,
		})
		return
	}
	name := accountName(fp)
	acc, err := s.cdp.GetAccountByName(name)
	if err != nil && !isNotFound(err) {
		writeError(w, -32603, "cdp lookup: "+err.Error())
		return
	}
	if acc == nil {
		acc, err = s.cdp.CreateAccount(name)
		if err != nil {
			writeError(w, -32603, "cdp create: "+err.Error())
			return
		}
	}
	if err := s.store.SaveWallet(&store.Wallet{
		Fingerprint: fp, Address: acc.Address, AccountName: acc.Name,
		Network: s.network, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	// A fresh CDP account starts with 0 ETH, so its very first transaction
	// (a USDC transfer / paid group join) would fail for lack of gas. On a
	// testnet, seed the account with faucet ETH right away so it works
	// out of the box. Best-effort: failures surface in the response, not as a
	// hard error (the user can still receive funds without ETH).
	fauceted := false
	faucetErr := ""
	if cdp.IsTestnet(s.network) {
		if _, err := s.cdp.RequestFaucet(s.network, acc.Address, "eth"); err != nil {
			faucetErr = err.Error()
		} else {
			fauceted = true
		}
	}
	resp := map[string]any{
		"address": acc.Address, "network": s.network, "created": true,
		"eth_seeded": fauceted,
	}
	if faucetErr != "" {
		resp["faucet_error"] = faucetErr
	}
	writeJSON(w, resp)
}

// walletBalance returns the wallet's USDC + ETH balances. With CDP it uses the
// token-balances API; in manual-address mode it queries the public RPC.
func (s *Service) walletBalance(w http.ResponseWriter, r *http.Request) {
	fp := fpOf(r)
	address := ""
	if wal, err := s.store.GetWallet(fp); err != nil {
		writeError(w, -32603, err.Error())
		return
	} else if wal != nil {
		address = wal.Address
	}
	if address == "" {
		if c, _ := s.store.GetConfig(); c != nil && isHexAddress(c.ManualAddress) {
			address = c.ManualAddress
		}
	}
	if address == "" {
		writeError(w, ErrNoWallet, "no wallet yet: create one in the wallet settings first")
		return
	}
	usdc, err := s.rpc.BalanceOf(r.Context(), mustUSDC(s.network), address)
	if err != nil {
		writeError(w, -32603, "balance: "+err.Error())
		return
	}
	eth, err := s.rpc.GetBalance(r.Context(), address)
	if err != nil {
		writeError(w, -32603, "balance: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"address": address, "network": s.network,
		"balance_usdc": atomicToDecimal(usdc, 6),
		"balance_eth":  atomicToDecimal(eth, 18),
	})
}

// walletBind binds an EXISTING wallet to the caller's fingerprint instead of
// creating a new CDP account. Two kinds are supported:
//
//   - address (manual-address): the caller's own external 0x address (MetaMask
//     etc.) becomes their receiving address; balance reads use the public RPC
//     and outgoing transfers are not available from this gateway (signing keys
//     live outside). The gateway mode is set to "manual-address".
//   - account_name (CDP): find an EXISTING CDP account in the user's project
//     by name and bind it (a wallet previously created under another
//     fingerprint, or created in the CDP dashboard). Requires CDP.
func (s *Service) walletBind(w http.ResponseWriter, r *http.Request) {
	fp := fpOf(r)
	existing, err := s.store.GetWallet(fp)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	if existing != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "already bound", "address": existing.Address})
		return
	}
	var req struct {
		Address     string `json:"address"`
		AccountName string `json:"account_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, ErrBadRequest, "bad json: "+err.Error())
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	name := strings.TrimSpace(req.AccountName)
	switch {
	case isHexAddress(addr) && name == "":
		// External address binding (manual-address mode): the user already owns
		// this address; this gateway only reads its balance / accepts payments.
		if err := s.store.SaveWallet(&store.Wallet{
			Fingerprint: fp, Address: addr, AccountName: "manual",
			Network: s.network, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			writeError(w, -32603, err.Error())
			return
		}
		if c, _ := s.store.GetConfig(); c != nil {
			c.Mode = "manual-address"
			c.ManualAddress = addr
			if err := s.store.SaveConfig(c); err != nil {
				writeError(w, -32603, err.Error())
				return
			}
			s.cfg.c = c
		}
		writeJSON(w, map[string]any{
			"ok": true, "bound": "address", "address": addr, "network": s.network,
		})
		return
	case addr == "" && name != "":
		if s.cdp == nil {
			writeError(w, ErrCDPNotConfigured, "CDP not configured: 设置 → 灵镜网络 → CDP")
			return
		}
		acc, err := s.cdp.GetAccountByName(name)
		if err != nil {
			if isNotFound(err) {
				writeError(w, ErrBadRequest, "account not found: "+name)
				return
			}
			writeError(w, -32603, "cdp lookup: "+err.Error())
			return
		}
		if err := s.store.SaveWallet(&store.Wallet{
			Fingerprint: fp, Address: acc.Address, AccountName: acc.Name,
			Network: s.network, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			writeError(w, -32603, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"ok": true, "bound": "cdp", "address": acc.Address, "account_name": acc.Name, "network": s.network,
		})
		return
	default:
		writeError(w, ErrBadRequest, "provide exactly one of: address (0x) or account_name")
		return
	}
}

// transfer sends USDC from the caller's CDP wallet to an on-chain address.
func (s *Service) transfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToAddress      string `json:"to_address"`
		AmountUSDC     string `json:"amount_usdc"`
		Memo           string `json:"memo,omitempty"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, ErrBadRequest, "bad json: "+err.Error())
		return
	}
	if s.cdp == nil {
		writeError(w, ErrCDPNotConfigured, "CDP not configured")
		return
	}
	if !isHexAddress(req.ToAddress) {
		writeError(w, ErrBadRequest, "to_address must be a 0x address")
		return
	}
	amountAtomic, err := decimalToAtomic(req.AmountUSDC, 6)
	if err != nil || amountAtomic.Sign() <= 0 {
		writeError(w, ErrBadRequest, "amount_usdc must be a positive decimal, e.g. \"1.00\"")
		return
	}

	fp := fpOf(r)
	wallet, err := s.store.GetWallet(fp)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	if wallet == nil {
		writeError(w, ErrNoWallet, "no wallet yet: create one in the wallet settings first")
		return
	}

	usdc, err := cdp.USDCContract(s.network)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	data, err := cdp.BuildERC20TransferData(req.ToAddress, amountAtomic)
	if err != nil {
		writeError(w, ErrBadRequest, err.Error())
		return
	}
	chainID, err := cdp.ChainID(s.network)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}

	// Fetch nonce + gas from the public RPC (CDP could also fill these, but
	// explicit values keep the RLP deterministic and the failure modes local).
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	nonce, err := s.rpc.GetTransactionCount(ctx, wallet.Address)
	if err != nil {
		writeError(w, -32603, "nonce: "+err.Error())
		return
	}
	priority, err := s.rpc.MaxPriorityFeePerGas(ctx)
	if err != nil {
		writeError(w, -32603, "priority fee: "+err.Error())
		return
	}
	gasPrice, err := s.rpc.GasPrice(ctx)
	if err != nil {
		writeError(w, -32603, "gas price: "+err.Error())
		return
	}
	maxFee := new(big.Int).Add(gasPrice, priority)
	gasLimit, err := s.rpc.EstimateGas(ctx, wallet.Address, usdc, data)
	if err != nil {
		writeError(w, -32603, "gas estimate: "+err.Error())
		return
	}

	tx := cdp.EIP1559Tx{
		ChainID:              chainID,
		Nonce:                nonce,
		MaxPriorityFeePerGas: priority,
		MaxFeePerGas:         maxFee,
		GasLimit:             gasLimit,
		To:                   usdc,
		Value:                big.NewInt(0),
		Data:                 data,
	}
	rlpHex, err := tx.RLP()
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	txHash, err := s.cdp.SendTransaction(wallet.Address, s.network, rlpHex)
	if err != nil {
		writeError(w, -32603, "send: "+err.Error())
		return
	}

	transfer := &store.Transfer{
		ID:        req.IdempotencyKey,
		FromFP:    fp,
		ToAddress: req.ToAddress,
		Amount:    req.AmountUSDC,
		TxHash:    txHash,
		Status:    "processing",
		Memo:      req.Memo,
	}
	if transfer.ID == "" {
		transfer.ID = "t-" + txHash[:18]
	}
	if err := s.store.AppendTransfer(transfer); err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"transfer_id": transfer.ID, "tx_hash": txHash,
		"from": wallet.Address, "to": req.ToAddress,
		"amount": req.AmountUSDC, "status": "processing",
	})
}

// joinVerify checks that a paid group-join transfer really happened: the tx is
// confirmed, paid to the group's join address, for at least the join price,
// and — the replay protection — that it is bound to the applicant:
//
//   - the tx's on-chain sender must match the applicant's declared payer;
//   - when a wallet-secret receipt (join.receipt) is attached, the ES256
//     signature must verify under the embedded P-256 public key and that key
//     must derive to the sender address (the applicant provably holds the
//     wallet that paid).
//
// It uses the public RPC — no CDP needed by the verifier.
func (s *Service) joinVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxHash string               `json:"tx_hash"`
		To     string               `json:"to"`
		Amount string               `json:"amount"`          // decimal USDC, e.g. "1.00"
		Payer  string               `json:"payer,omitempty"` // declared paying 0x address (identity binding)
		Proof  *JoinPaymentProofReq `json:"proof,omitempty"` // wallet-secret receipt
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, ErrBadRequest, "bad json: "+err.Error())
		return
	}
	if !strings.HasPrefix(req.TxHash, "0x") || len(req.TxHash) != 66 {
		writeError(w, ErrBadRequest, "tx_hash must be a 0x transaction hash")
		return
	}
	if !isHexAddress(req.To) {
		writeError(w, ErrBadRequest, "to must be a 0x address")
		return
	}
	minAmount, err := decimalToAtomic(req.Amount, 6)
	if err != nil || minAmount.Sign() <= 0 {
		writeError(w, ErrBadRequest, "amount must be a positive decimal, e.g. \"1.00\"")
		return
	}

	usdc, err := cdp.USDCContract(s.network)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ok, actual, from, err := s.rpc.VerifyUSDCTransfer(ctx, req.TxHash, usdc, req.To, minAmount)
	if err != nil {
		writeError(w, -32603, "verify: "+err.Error())
		return
	}
	if !ok {
		reason := "transaction not found or not yet confirmed"
		if actual != nil {
			reason = "received " + atomicToDecimal(actual, 6) + " USDC — less than the join price"
		}
		writeJSON(w, map[string]any{"valid": false, "reason": reason})
		return
	}
	// Identity binding: the on-chain sender must be the applicant's payer.
	if req.Payer != "" && !strings.EqualFold(from, req.Payer) {
		writeJSON(w, map[string]any{"valid": false, "reason": "payer mismatch: the transaction was sent from " + from + ", not the declared " + req.Payer})
		return
	}
	if req.Proof != nil {
		payer, err := verifyJoinReceipt(req.Proof)
		if err != nil {
			writeJSON(w, map[string]any{"valid": false, "reason": "invalid payment receipt: " + err.Error()})
			return
		}
		if !strings.EqualFold(payer, from) {
			writeJSON(w, map[string]any{"valid": false, "reason": "payment receipt is not signed by the transaction's sender"})
			return
		}
	}
	writeJSON(w, map[string]any{"valid": true, "amount": atomicToDecimal(actual, 6), "from": from})
}

// JoinPaymentProofReq is the wallet-secret receipt an applicant attaches to a
// paid-join application (a2a.JoinPayment.Proof): a signed statement
// {"fp","tx_hash","payer"} proving control of the paying wallet.
type JoinPaymentProofReq struct {
	Message string `json:"message"` // canonical JSON {"fp","tx_hash","payer"}
	Pubkey  string `json:"pubkey"`  // hex 0x04||x||y (uncompressed P-256 public key)
	Sig     string `json:"sig"`     // hex R||S (raw ES256 signature)
}

// joinReceipt mints the wallet-secret receipt for a paid-join payment: the
// caller proves control of the wallet that sent tx_hash by signing
// {"fp","tx_hash","payer"} with the wallet secret (ES256, P-256). The
// group owner's node verifies it in join.verify against the on-chain sender.
func (s *Service) joinReceipt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxHash string `json:"tx_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, ErrBadRequest, "bad json: "+err.Error())
		return
	}
	if !strings.HasPrefix(req.TxHash, "0x") || len(req.TxHash) != 66 {
		writeError(w, ErrBadRequest, "tx_hash must be a 0x transaction hash")
		return
	}
	if s.cdp == nil {
		writeError(w, ErrCDPNotConfigured, "CDP not configured")
		return
	}
	fp := fpOf(r)
	wallet, err := s.store.GetWallet(fp)
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	if wallet == nil {
		writeError(w, ErrNoWallet, "no wallet for this identity yet")
		return
	}
	// encoding/json marshals map keys in sorted order, so the message bytes are
	// canonical and the verifier re-hashes them verbatim.
	msgBytes, err := json.Marshal(map[string]string{"fp": fp, "tx_hash": req.TxHash, "payer": wallet.Address})
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	sig, err := s.cdp.SignWalletMessage(msgBytes)
	if err != nil {
		writeError(w, -32603, "wallet sign: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"message": string(msgBytes),
		"pubkey":  hex.EncodeToString(s.cdp.WalletPublicKey()),
		"sig":     hex.EncodeToString(sig),
	})
}

// verifyJoinReceipt checks a wallet-secret receipt: the ES256 signature must
// verify under the embedded P-256 public key, and the EVM address derived from
// that key must equal the receipt's declared payer. It returns the payer.
func verifyJoinReceipt(p *JoinPaymentProofReq) (string, error) {
	if p == nil {
		return "", fmt.Errorf("missing receipt")
	}
	var msg struct {
		FP     string `json:"fp"`
		TxHash string `json:"tx_hash"`
		Payer  string `json:"payer"`
	}
	if err := json.Unmarshal([]byte(p.Message), &msg); err != nil {
		return "", fmt.Errorf("message: %w", err)
	}
	if !isHexAddress(msg.Payer) {
		return "", fmt.Errorf("receipt payer is not a 0x address")
	}
	pub, err := parseUncompressedP256(p.Pubkey)
	if err != nil {
		return "", fmt.Errorf("pubkey: %w", err)
	}
	sig, err := hex.DecodeString(p.Sig)
	if err != nil || len(sig) != 64 {
		return "", fmt.Errorf("sig must be a 64-byte hex R||S signature")
	}
	digest := sha256.Sum256([]byte(p.Message))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "", fmt.Errorf("signature does not verify under the embedded public key")
	}
	if !strings.EqualFold(cdp.EVMAddressFromPublicKey(pub.X, pub.Y), msg.Payer) {
		return "", fmt.Errorf("public key does not derive to the declared payer")
	}
	return msg.Payer, nil
}

// parseUncompressedP256 parses a hex 0x04||x||y uncompressed P-256 public key.
func parseUncompressedP256(s string) (*ecdsa.PublicKey, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 65 || raw[0] != 4 {
		return nil, fmt.Errorf("expected an uncompressed 0x04||x||y P-256 public key")
	}
	x := new(big.Int).SetBytes(raw[1:33])
	y := new(big.Int).SetBytes(raw[33:65])
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("public key is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// setConfig persists the gateway mode (manual address / network). CDP secrets
// are configured out-of-band (env / keychain) and never pass through here.
func (s *Service) setConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode          string `json:"mode"`
		ManualAddress string `json:"manual_address,omitempty"`
		Network       string `json:"network,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, ErrBadRequest, "bad json: "+err.Error())
		return
	}
	c, err := s.store.GetConfig()
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	if req.Mode != "" {
		if req.Mode != "manual-address" && req.Mode != "local-cdp" && req.Mode != "remote-gateway" {
			writeError(w, ErrBadRequest, "mode must be manual-address | local-cdp | remote-gateway")
			return
		}
		c.Mode = req.Mode
	}
	if req.ManualAddress != "" {
		if !isHexAddress(req.ManualAddress) {
			writeError(w, ErrBadRequest, "manual_address must be a 0x address")
			return
		}
		c.ManualAddress = strings.ToLower(req.ManualAddress)
	}
	if req.Network != "" {
		if _, err := cdp.USDCContract(req.Network); err != nil {
			writeError(w, ErrBadRequest, err.Error())
			return
		}
		c.Network = req.Network
		s.network = req.Network
		if s.rpc != nil {
			if u, err := cdp.RPCEndpoint(req.Network); err == nil {
				s.rpc = rpcclient.New(u)
			}
		}
	}
	if err := s.store.SaveConfig(c); err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	s.cfg.c = c
	writeJSON(w, map[string]any{"ok": true, "mode": c.Mode, "network": c.Network})
}

func (s *Service) getConfig(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetConfig()
	if err != nil {
		writeError(w, -32603, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"mode": c.Mode, "network": c.Network, "manual_address": c.ManualAddress,
		"cdp_configured": s.cdp != nil,
	})
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	c, _ := s.store.GetConfig()
	writeJSON(w, map[string]any{"ok": true, "network": s.network, "mode": c.Mode, "cdp_configured": s.cdp != nil})
}

// ——— helpers ———

// accountName derives a CDP-unique account name from a fingerprint
// (pattern ^[A-Za-z0-9][A-Za-z0-9-]{0,34}[A-Za-z0-9]$; '_' and '=' are dropped).
func accountName(fp string) string {
	var b strings.Builder
	b.WriteString("fp-")
	for _, ch := range fp {
		if ch == '_' || ch == '=' {
			continue
		}
		b.WriteRune(ch)
	}
	name := b.String()
	if len(name) > 36 {
		name = name[:36]
	}
	return name
}

func isHexAddress(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(s) != 40 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

// decimalToAtomic converts a decimal string like "1.25" to atomic units (×10^dec).
func decimalToAtomic(dec string, decimals int) (*big.Int, error) {
	dec = strings.TrimSpace(dec)
	if dec == "" {
		return nil, fmt.Errorf("empty amount")
	}
	neg := strings.HasPrefix(dec, "-")
	dec = strings.TrimPrefix(strings.TrimPrefix(dec, "-"), "+")
	parts := strings.SplitN(dec, ".", 2)
	whole := parts[0]
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if frac == "" && whole == "" {
		return nil, fmt.Errorf("bad amount")
	}
	if len(frac) > decimals {
		return nil, fmt.Errorf("too many decimals")
	}
	frac = frac + strings.Repeat("0", decimals-len(frac))
	num := new(big.Int)
	if _, ok := num.SetString(whole+frac, 10); !ok {
		return nil, fmt.Errorf("bad amount")
	}
	if neg {
		num.Neg(num)
	}
	return num, nil
}

// atomicToDecimal renders atomic units as a decimal string.
func atomicToDecimal(atomic *big.Int, decimals int) string {
	neg := atomic.Sign() < 0
	a := new(big.Int).Abs(atomic)
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole := new(big.Int).Div(a, pow)
	frac := new(big.Int).Mod(a, pow)
	fs := fmt.Sprintf("%0*d", decimals, frac)
	fs = strings.TrimRight(fs, "0")
	if fs == "" {
		fs = "0"
	}
	out := whole.String() + "." + fs
	if neg {
		out = "-" + out
	}
	return out
}

func mustUSDC(network string) string {
	addr, err := cdp.USDCContract(network)
	if err != nil {
		panic(err)
	}
	return addr
}

// isNotFound reports whether a CDP client error is an HTTP 404.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http 404")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "code": code})
}
