package cdp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BaseURL is the CDP v2 platform API base (openapi.yaml servers[0]).
const BaseURL = "https://api.cdp.coinbase.com/platform"

// Account is the subset of EvmAccount we need.
type Account struct {
	Address   string   `json:"address"`
	Name      string   `json:"name,omitempty"`
	Policies  []string `json:"policies,omitempty"`
	CreatedAt string   `json:"createdAt,omitempty"`
}

// TokenBalance is one entry of the token-balances response.
type TokenBalance struct {
	Amount struct {
		Amount   string `json:"amount"`
		Decimals int    `json:"decimals"`
	} `json:"amount"`
	Token struct {
		Network         string `json:"network"`
		Symbol          string `json:"symbol"`
		Name            string `json:"name"`
		ContractAddress string `json:"contractAddress"`
	} `json:"token"`
}

// Client is a minimal CDP v2 REST client.
type Client struct {
	http    *http.Client
	auth    *auth
	baseURL string
}

// NewClient builds a CDP client from credentials.
func NewClient(cred Credentials) (*Client, error) {
	a, err := newAuth(cred)
	if err != nil {
		return nil, err
	}
	return &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		auth:    a,
		baseURL: BaseURL,
	}, nil
}

// CreateAccount creates a server-signer EVM account (POST /v2/evm/accounts).
func (c *Client) CreateAccount(name string) (*Account, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	var out Account
	if err := c.do(http.MethodPost, "/v2/evm/accounts", body, true, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccountByAddress fetches one EVM account (GET /v2/evm/accounts/{address}).
// Uses the exact address (case-sensitive lookup, same as CreateAccount returns).
func (c *Client) GetAccountByAddress(address string) (*Account, error) {
	var out Account
	if err := c.do(http.MethodGet, "/v2/evm/accounts/"+address, nil, false, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccountByName fetches one EVM account by its unique name.
func (c *Client) GetAccountByName(name string) (*Account, error) {
	var out Account
	if err := c.do(http.MethodGet, "/v2/evm/accounts/by-name/"+name, nil, false, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTokenBalances returns token balances for an address on a network
// (GET /v2/evm/token-balances/{network}/{address}).
func (c *Client) ListTokenBalances(network, address string) ([]TokenBalance, error) {
	var out struct {
		Balances []TokenBalance `json:"balances"`
	}
	if err := c.do(http.MethodGet, "/v2/evm/token-balances/"+network+"/"+strings.ToLower(address), nil, false, &out); err != nil {
		return nil, err
	}
	return out.Balances, nil
}

// SendTransaction signs and sends an unsigned EIP-1559 transaction
// (POST /v2/evm/accounts/{address}/send/transaction). Requires wallet auth.
// The address must be the exact account address as returned by CreateAccount —
// the API's account lookup for this endpoint is case-sensitive.
func (c *Client) SendTransaction(address, network, rlpHex string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"network":     network,
		"transaction": rlpHex,
	})
	var out struct {
		TransactionHash string `json:"transactionHash"`
	}
	if err := c.do(http.MethodPost, "/v2/evm/accounts/"+address+"/send/transaction", body, true, &out); err != nil {
		return "", err
	}
	if out.TransactionHash == "" {
		return "", fmt.Errorf("send/transaction: empty transactionHash")
	}
	return out.TransactionHash, nil
}

// RequestFaucet requests testnet funds (eth | usdc | eurc | cbbtc) from the CDP
// faucet on a test network (POST /v2/evm/faucet). Testnets only.
func (c *Client) RequestFaucet(network, address, token string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"network": network,
		"address": strings.ToLower(address),
		"token":   token,
	})
	var out struct {
		TransactionHash string `json:"transactionHash"`
	}
	if err := c.do(http.MethodPost, "/v2/evm/faucet", body, false, &out); err != nil {
		return "", err
	}
	if out.TransactionHash == "" {
		return "", fmt.Errorf("faucet: empty transactionHash")
	}
	return out.TransactionHash, nil
}

// do performs one authenticated request. walletAuth enables the X-Wallet-Auth
// header (required for sign/send endpoints).
func (c *Client) do(method, path string, body []byte, walletAuth bool, out any) error {
	host := strings.TrimPrefix(c.baseURL, "https://")
	url := c.baseURL + path

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	jwt, err := c.auth.platformJWT(method, host, path)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)

	if walletAuth {
		wjwt, err := c.auth.walletJWT(method, host, path, body)
		if err != nil {
			return err
		}
		req.Header.Set("X-Wallet-Auth", wjwt)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cdp %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cdp %s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("cdp %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// WalletPublicKey returns the wallet's uncompressed P-256 public key, or nil
// when CDP / the wallet secret is not configured.
func (c *Client) WalletPublicKey() []byte {
	if c.auth == nil {
		return nil
	}
	return c.auth.WalletPublicKey()
}

// SignWalletMessage signs a message with the wallet secret (raw ES256 R||S,
// 64 bytes) — proof that the caller holds the wallet key. Used to mint
// paid-join receipts so the receiver can bind a payment to the payer.
func (c *Client) SignWalletMessage(msg []byte) ([]byte, error) {
	if c.auth == nil {
		return nil, fmt.Errorf("CDP not configured")
	}
	return c.auth.SignWalletSecret(msg)
}
