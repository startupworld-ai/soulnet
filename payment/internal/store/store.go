// Package store persists the gateway's local state under <home>/a2a/pay/:
//
//	wallets.json   fp → CDP EVM account (created under the user's own project)
//	transfers.jsonl  one line per transfer (ledger)
//	config.json    mode (manual-address / local-cdp / remote-gateway) + settings
//
// CDP secrets never live here: they are read from the environment / system
// keychain by the config package.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is a small file-backed store, one file per concern, guarded by a mutex.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open creates the store directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Wallet is the local binding between an A2A fingerprint and a CDP EVM account.
type Wallet struct {
	Fingerprint string `json:"fp"`
	Address     string `json:"evm_address"`
	AccountName string `json:"account_name"`
	Network     string `json:"network"`
	CreatedAt   string `json:"created_at"`
}

// Transfer is one ledger entry (transfers.jsonl).
type Transfer struct {
	ID        string `json:"id"`
	FromFP    string `json:"from_fp,omitempty"`
	ToAddress string `json:"to_address"`
	Amount    string `json:"amount"` // decimal USDC
	TxHash    string `json:"tx_hash"`
	Status    string `json:"status"` // quoted|processing|completed|failed
	Memo      string `json:"memo,omitempty"`
	TS        string `json:"ts"`
}

// Config is the gateway's own mode configuration (no secrets).
type Config struct {
	Mode          string `json:"mode"` // "manual-address" | "local-cdp" | "remote-gateway"
	GatewayURL    string `json:"gateway_url,omitempty"`
	ManualAddress string `json:"manual_address,omitempty"`
	Network       string `json:"network,omitempty"` // base-sepolia | base
}

// GetWallet returns the wallet bound to a fingerprint, or nil.
func (s *Store) GetWallet(fp string) (*Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readWallets()
	if err != nil {
		return nil, err
	}
	if w, ok := all[fp]; ok {
		cp := w
		return &cp, nil
	}
	return nil, nil
}

// SaveWallet upserts the wallet binding for a fingerprint.
func (s *Store) SaveWallet(w *Wallet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readWallets()
	if err != nil {
		return err
	}
	all[w.Fingerprint] = *w
	return s.writeWallets(all)
}

func (s *Store) readWallets() (map[string]Wallet, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, "wallets.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Wallet{}, nil
		}
		return nil, err
	}
	out := map[string]Wallet{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("wallets.json: %w", err)
	}
	return out, nil
}

func (s *Store) writeWallets(all map[string]Wallet) error {
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "wallets.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "wallets.json"))
}

// AppendTransfer appends one ledger line.
func (s *Store) AppendTransfer(t *Transfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.TS == "" {
		t.TS = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "transfers.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// GetConfig loads the mode config (defaults: manual-address, network base-sepolia).
func (s *Store) GetConfig() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Mode: "manual-address", Network: "base-sepolia"}, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Network == "" {
		c.Network = "base-sepolia"
	}
	return &c, nil
}

// SaveConfig persists the mode config.
func (s *Store) SaveConfig(c *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "config.json"), raw, 0o600)
}
