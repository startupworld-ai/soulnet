package cdp

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// Network identifiers used by the CDP v2 API and their chain ids / RPC endpoints.
const (
	NetworkBaseSepolia = "base-sepolia"
	NetworkBase        = "base"

	ChainIDBaseSepolia = 84532
	ChainIDBase        = 8453

	// USDC contract addresses (6 decimals).
	USDCBaseSepolia = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
	USDCBase        = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"

	// Public RPC endpoints (free, no key needed) for gas estimation and tx verification.
	RPCSepolia = "https://sepolia.base.org"
	RPCMainnet = "https://mainnet.base.org"
)

// IsTestnet reports whether the network is a test network (the CDP faucet only
// serves testnets; mainnet must never call it).
func IsTestnet(network string) bool {
	return network == NetworkBaseSepolia
}

// USDCContract returns the USDC address for a CDP network name.
func USDCContract(network string) (string, error) {
	switch network {
	case NetworkBaseSepolia:
		return USDCBaseSepolia, nil
	case NetworkBase:
		return USDCBase, nil
	default:
		return "", fmt.Errorf("unsupported network %q", network)
	}
}

// ChainID returns the chain id for a CDP network name.
func ChainID(network string) (int64, error) {
	switch network {
	case NetworkBaseSepolia:
		return ChainIDBaseSepolia, nil
	case NetworkBase:
		return ChainIDBase, nil
	default:
		return 0, fmt.Errorf("unsupported network %q", network)
	}
}

// RPCEndpoint returns the public RPC endpoint for a CDP network name.
func RPCEndpoint(network string) (string, error) {
	switch network {
	case NetworkBaseSepolia:
		return RPCSepolia, nil
	case NetworkBase:
		return RPCMainnet, nil
	default:
		return "", fmt.Errorf("unsupported network %q", network)
	}
}

// transferSelector is keccak256("transfer(address,uint256)")[0:4] = a9059cbb.
var transferSelector = mustHex("a9059cbb")

// BuildERC20TransferData encodes an ERC-20 transfer(to, amount) calldata.
// amount is in the token's atomic units (USDC: 6 decimals).
func BuildERC20TransferData(to string, amount *big.Int) (string, error) {
	addr := strings.ToLower(strings.TrimPrefix(to, "0x"))
	if len(addr) != 40 {
		return "", fmt.Errorf("invalid to address %q", to)
	}
	toBytes, err := hex.DecodeString(addr)
	if err != nil {
		return "", err
	}
	amountBytes := make([]byte, 32)
	amount.FillBytes(amountBytes)
	out := append([]byte{}, transferSelector...)
	out = append(out, make([]byte, 12)...) // pad address to 32 bytes
	out = append(out, toBytes...)
	out = append(out, amountBytes...)
	return "0x" + hex.EncodeToString(out), nil
}

// BuildEIP1559UnsignedTx builds the unsigned EIP-1559 transaction RLP that the
// CDP send/transaction API accepts (it signs and fills nonce/gas on its side).
//
//	0x02 || rlp([chainId, nonce, maxPriorityFeePerGas, maxFeePerGas, gasLimit,
//	             to, value, data, accessList])
//
// Gas fields are passed through: use 0 when you want CDP to estimate, or real
// values when you fetched them from an RPC.
type EIP1559Tx struct {
	ChainID              int64
	Nonce                uint64
	MaxPriorityFeePerGas *big.Int
	MaxFeePerGas         *big.Int
	GasLimit             uint64
	To                   string // 0x-prefixed
	Value                *big.Int
	Data                 string // 0x-prefixed calldata
}

// RLP returns the 0x02-prefixed RLP hex string of the unsigned transaction.
func (t *EIP1559Tx) RLP() (string, error) {
	to := strings.ToLower(strings.TrimPrefix(t.To, "0x"))
	toBytes, err := hex.DecodeString(to)
	if err != nil {
		return "", fmt.Errorf("invalid to: %w", err)
	}
	data := strings.TrimPrefix(t.Data, "0x")
	dataBytes, err := hex.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("invalid data: %w", err)
	}
	fields := []any{
		big.NewInt(t.ChainID),
		t.Nonce,
		t.MaxPriorityFeePerGas,
		t.MaxFeePerGas,
		t.GasLimit,
		toBytes,
		t.Value,
		dataBytes,
		[]any{}, // access list
		[]any{}, // y parity
		[]any{}, // r
		[]any{}, // s
	}
	// Note: the EIP-1559 payload for an *unsigned* transaction is
	// rlp([chainId, nonce, maxPriority, maxFee, gas, to, value, data, [], []])
	// — the trailing yParity/r/s slots are filled by the signer and are empty here.
	payload := fields[:9]
	return "0x02" + hex.EncodeToString(rlpEncode(payload)), nil
}

// rlpEncode encodes a value per the Ethereum RLP rules.
func rlpEncode(v any) []byte {
	switch t := v.(type) {
	case uint64:
		return rlpEncodeUint(t)
	case *big.Int:
		if t == nil || t.Sign() == 0 {
			return []byte{0x80}
		}
		return rlpEncodeBytes(t.Bytes())
	case []byte:
		return rlpEncodeBytes(t)
	case string:
		return rlpEncodeBytes([]byte(t))
	case []any:
		var out []byte
		for _, e := range t {
			out = append(out, rlpEncode(e)...)
		}
		return appendLengthPrefix(out, 0xc0)
	default:
		panic(fmt.Sprintf("rlp: unsupported type %T", v))
	}
}

func rlpEncodeUint(n uint64) []byte {
	if n == 0 {
		return []byte{0x80}
	}
	return rlpEncodeBytes(big.NewInt(int64(n)).Bytes())
}

func rlpEncodeBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return b
	}
	return appendLengthPrefix(b, 0x80)
}

// appendLengthPrefix prepends the RLP length prefix for a payload with the given
// short-string/short-list offset (0x80 for bytes, 0xc0 for lists).
func appendLengthPrefix(payload []byte, offset byte) []byte {
	switch n := len(payload); {
	case n <= 55:
		return append([]byte{offset + byte(n)}, payload...)
	default:
		lenBytes := uintToBytes(uint64(n))
		prefix := append([]byte{offset + 55 + byte(len(lenBytes))}, lenBytes...)
		return append(prefix, payload...)
	}
}

func uintToBytes(n uint64) []byte {
	return new(big.Int).SetUint64(n).Bytes()
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
