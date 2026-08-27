package cdp

import (
	"math/big"
	"strings"
	"testing"
)

// Vectors computed with an independent Python RLP implementation following
// the Ethereum RLP spec (minimal-bytes length prefixes).

func TestBuildERC20TransferData(t *testing.T) {
	got, err := BuildERC20TransferData("0x1234567890abcdef1234567890abcdef12345678", big.NewInt(1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	want := "0xa9059cbb0000000000000000000000001234567890abcdef1234567890abcdef1234567800000000000000000000000000000000000000000000000000000000000f4240"
	if got != want {
		t.Fatalf("data mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestBuildERC20TransferDataBadAddress(t *testing.T) {
	if _, err := BuildERC20TransferData("0x1234", big.NewInt(1)); err == nil {
		t.Fatal("expected error for short address")
	}
}

func TestEIP1559TxRLP(t *testing.T) {
	cases := []struct {
		name string
		tx   EIP1559Tx
		want string
	}{
		{
			name: "eip1559 spec example",
			tx: EIP1559Tx{
				ChainID: 4, Nonce: 9,
				MaxPriorityFeePerGas: big.NewInt(0x4a817c800),
				MaxFeePerGas:         big.NewInt(0x4a817c808),
				GasLimit:             0x5208,
				To:                   "0x3535353535353535353535353535353535353535",
				Value:                big.NewInt(0xde0b6b3a7640000),
				Data:                 "0x",
			},
			want: "0x02f104098504a817c8008504a817c808825208943535353535353535353535353535353535353535880de0b6b3a764000080c0",
		},
		{
			name: "base sepolia usdc transfer zero fees",
			tx: EIP1559Tx{
				ChainID:              84532,
				Nonce:                0,
				MaxPriorityFeePerGas: big.NewInt(0),
				MaxFeePerGas:         big.NewInt(0),
				GasLimit:             0,
				To:                   USDCBaseSepolia,
				Value:                big.NewInt(0),
				Data:                 "0xa9059cbb0000000000000000000000001234567890abcdef1234567890abcdef1234567800000000000000000000000000000000000000000000000000000000000f4240",
			},
			want: "0x02f86583014a348080808094036cbd53842c5426634e7929541ec2318f3dcf7e80b844a9059cbb0000000000000000000000001234567890abcdef1234567890abcdef1234567800000000000000000000000000000000000000000000000000000000000f4240c0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.tx.RLP()
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("rlp mismatch:\n got %s\nwant %s", got, c.want)
			}
			if !strings.HasPrefix(got, "0x02") {
				t.Fatalf("expected EIP-1559 envelope 0x02, got %s", got)
			}
		})
	}
}

func TestIsTestnet(t *testing.T) {
	cases := []struct {
		network string
		want    bool
	}{
		{NetworkBaseSepolia, true},
		{NetworkBase, false},
		{"", false},
		{"ethereum", false},
	}
	for _, c := range cases {
		if got := IsTestnet(c.network); got != c.want {
			t.Fatalf("IsTestnet(%q) = %v, want %v", c.network, got, c.want)
		}
	}
}
