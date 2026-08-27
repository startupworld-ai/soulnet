// Package rpcclient is a minimal JSON-RPC client for public Base RPC endpoints.
// It is used for (a) verifying that a paid group-join transfer really arrived
// (join.verify) and (b) fetching nonce/gas values for building transactions.
// No API key is required — the endpoints are public.
package rpcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// TransferTopic is keccak256("Transfer(address,address,uint256)").
const TransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

// Client is a JSON-RPC 2.0 client for one public RPC endpoint.
type Client struct {
	endpoint string
	http     *http.Client
	seq      int
}

// New creates a client for the given RPC endpoint (e.g. https://sepolia.base.org).
func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Receipt is the subset of eth_getTransactionReceipt we need.
type Receipt struct {
	Status      string `json:"status"` // "0x1" = success
	BlockNumber string `json:"blockNumber"`
	// From is the sender of the transaction (the payer of a USDC transfer).
	From string `json:"from"`
	Logs []Log  `json:"logs"`
}

// Log is one event log.
type Log struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

// GetTransactionReceipt returns the receipt, or nil when the tx is not found yet.
func (c *Client) GetTransactionReceipt(ctx context.Context, txHash string) (*Receipt, error) {
	var out Receipt
	found, err := c.call(ctx, "eth_getTransactionReceipt", []any{txHash}, &out)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &out, nil
}

// GetTransactionCount returns the pending nonce of an address.
func (c *Client) GetTransactionCount(ctx context.Context, address string) (uint64, error) {
	var s string
	if _, err := c.call(ctx, "eth_getTransactionCount", []any{address, "pending"}, &s); err != nil {
		return 0, err
	}
	return hexToUint64(s)
}

// MaxPriorityFeePerGas returns eth_maxPriorityFeePerGas.
func (c *Client) MaxPriorityFeePerGas(ctx context.Context) (*big.Int, error) {
	var s string
	if _, err := c.call(ctx, "eth_maxPriorityFeePerGas", []any{}, &s); err != nil {
		return nil, err
	}
	return hexToBig(s)
}

// GasPrice returns eth_gasPrice.
func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	var s string
	if _, err := c.call(ctx, "eth_gasPrice", []any{}, &s); err != nil {
		return nil, err
	}
	return hexToBig(s)
}

// EstimateGas returns eth_estimateGas for a call.
func (c *Client) EstimateGas(ctx context.Context, from, to, data string) (uint64, error) {
	var s string
	if _, err := c.call(ctx, "eth_estimateGas", []any{map[string]string{
		"from": from, "to": to, "data": data, "value": "0x0",
	}}, &s); err != nil {
		return 0, err
	}
	return hexToUint64(s)
}

// VerifyUSDCTransfer verifies that txHash is a confirmed USDC transfer to `to`
// of at least `minAmount` atomic units, on the given USDC contract. It returns
// (verified, actualAmount, sender, err). `sender` is the tx's `from` (the
// payer) when the transfer is found, "" otherwise — the receiver needs it to
// bind the proof to the applicant's identity. A nil receipt (unconfirmed/not
// found) is a normal "not yet" outcome: verified=false, err=nil.
func (c *Client) VerifyUSDCTransfer(ctx context.Context, txHash, usdcContract, to string, minAmount *big.Int) (bool, *big.Int, string, error) {
	receipt, err := c.GetTransactionReceipt(ctx, txHash)
	if err != nil {
		return false, nil, "", err
	}
	if receipt == nil {
		return false, nil, "", nil // not found / pending
	}
	if receipt.Status != "0x1" {
		return false, nil, "", nil // reverted
	}
	want := strings.ToLower(strings.TrimPrefix(to, "0x"))
	if len(want) != 40 {
		return false, nil, "", fmt.Errorf("invalid to address %q", to)
	}
	for _, log := range receipt.Logs {
		if !strings.EqualFold(log.Address, usdcContract) {
			continue
		}
		if len(log.Topics) < 3 || !strings.EqualFold(log.Topics[0], TransferTopic) {
			continue
		}
		// topics[1]=from, topics[2]=to (32-byte left-padded), data=amount (uint256)
		toTopic := strings.TrimPrefix(log.Topics[2], "0x")
		if len(toTopic) != 64 || !strings.EqualFold(toTopic[24:], want) {
			continue
		}
		amount, ok := new(big.Int).SetString(strings.TrimPrefix(log.Data, "0x"), 16)
		if !ok {
			continue
		}
		if minAmount != nil && amount.Cmp(minAmount) < 0 {
			return false, amount, "", nil
		}
		return true, amount, receipt.From, nil
	}
	return false, nil, "", nil
}

// BalanceOf returns the ERC-20 balance of `holder` for `tokenContract` (eth_call).
func (c *Client) BalanceOf(ctx context.Context, tokenContract, holder string) (*big.Int, error) {
	// balanceOf(address) selector
	const sel = "0x70a08231"
	padded := strings.ToLower(strings.TrimPrefix(holder, "0x"))
	if len(padded) != 40 {
		return nil, fmt.Errorf("invalid holder %q", holder)
	}
	data := sel + strings.Repeat("0", 24) + padded
	var out string
	if _, err := c.call(ctx, "eth_call", []any{map[string]string{
		"to": tokenContract, "data": data,
	}, "latest"}, &out); err != nil {
		return nil, err
	}
	return hexToBig(out)
}

// GetBalance returns the native ETH balance of an address (eth_getBalance).
func (c *Client) GetBalance(ctx context.Context, address string) (*big.Int, error) {
	var out string
	if _, err := c.call(ctx, "eth_getBalance", []any{address, "latest"}, &out); err != nil {
		return nil, err
	}
	return hexToBig(out)
}

// call performs one JSON-RPC request. ok=false when the result is JSON null.
func (c *Client) call(ctx context.Context, method string, params []any, out any) (bool, error) {
	c.seq++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.seq,
		"method":  method,
		"params":  params,
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("rpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("rpc %s: http %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("rpc %s: decode: %w", method, err)
	}
	if envelope.Error != nil {
		return false, fmt.Errorf("rpc %s: %s", method, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return false, nil
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return false, fmt.Errorf("rpc %s: result decode: %w", method, err)
		}
	}
	return true, nil
}

func hexToBig(s string) (*big.Int, error) {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if s == "" {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil, fmt.Errorf("bad hex %q", s)
	}
	return v, nil
}

func hexToUint64(s string) (uint64, error) {
	v, err := hexToBig(s)
	if err != nil {
		return 0, err
	}
	if !v.IsUint64() {
		return 0, fmt.Errorf("overflow %q", s)
	}
	return v.Uint64(), nil
}
