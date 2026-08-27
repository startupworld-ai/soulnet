// Package config loads the gateway's runtime configuration. CDP secrets are
// read from environment variables (never from files in the repo) so that the
// open-source repository never contains credentials:
//
//	CDP_API_KEY_ID      platform API key id
//	CDP_API_KEY_SECRET  base64 Ed25519 (64B) or PEM EC P-256 private key
//	CDP_WALLET_SECRET   base64 DER EC P-256 PKCS8 private key
//	CDP_NETWORK         base-sepolia (default) | base
//	PAYGATE_LISTEN      listen address, default 127.0.0.1:9001
//	PAYGATE_HOME        data dir, default $HOME/.soulmirror/a2a/pay
//	PAYGATE_IDENTITY_FILE  path to the local identity.json; when present the
//	                       gateway only accepts A2A signatures from that
//	                       identity's fingerprint
package config

import (
	"os"

	"github.com/startupworld-ai/soulnet/payment/internal/cdp"
)

// Config is the gateway runtime configuration.
type Config struct {
	Listen  string
	HomeDir string
	Network string
	// IdentityFile is the path to the local identity.json (optional). When it
	// exists the gateway pins the caller to that identity's fingerprint.
	IdentityFile string
	CDP          cdp.Credentials
	// CDPConfigured is true when all three secrets are present.
	CDPConfigured bool
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	home := os.Getenv("PAYGATE_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = userHome + "/.soulmirror/a2a/pay"
	}
	listen := os.Getenv("PAYGATE_LISTEN")
	if listen == "" {
		listen = "127.0.0.1:9001"
	}
	network := os.Getenv("CDP_NETWORK")
	if network == "" {
		network = cdp.NetworkBaseSepolia
	}
	identityFile := os.Getenv("PAYGATE_IDENTITY_FILE")
	if identityFile == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		identityFile = userHome + "/.soulmirror/a2a/identity.json"
	}
	cred := cdp.Credentials{
		APIKeyID:     os.Getenv("CDP_API_KEY_ID"),
		APIKeySecret: os.Getenv("CDP_API_KEY_SECRET"),
		WalletSecret: os.Getenv("CDP_WALLET_SECRET"),
	}
	return &Config{
		Listen:        listen,
		HomeDir:       home,
		Network:       network,
		IdentityFile:  identityFile,
		CDP:           cred,
		CDPConfigured: cred.APIKeyID != "" && cred.APIKeySecret != "" && cred.WalletSecret != "",
	}, nil
}
