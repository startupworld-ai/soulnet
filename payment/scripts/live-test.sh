#!/usr/bin/env bash
# Live CDP integration test (Base Sepolia).
#
# Run from the repo root in YOUR OWN terminal (not inside a restricted sandbox):
#
#   bash payment/scripts/live-test.sh
#
# It sources ~/.soulmirror/a2a/pay/.env locally (secrets never leave this
# machine), then creates two test wallets, faucets USDC + ETH, sends a real
# USDC transfer through the full pipeline and verifies it on-chain.
#
# Exit code 0 = full pipeline OK. Output shows wallet addresses + tx hashes
# (safe to paste back; the secrets themselves are never printed).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENV_FILE="${PAYGATE_ENV_FILE:-$HOME/.soulmirror/a2a/pay/.env}"
GO="$ROOT/_tools/go/bin/go"

if [ ! -x "$GO" ]; then
  echo "error: Go 1.25 toolchain not found at $GO (expected from _tools/)" >&2
  exit 1
fi
if [ ! -f "$ENV_FILE" ]; then
  echo "error: env file not found at $ENV_FILE (PAYGATE_ENV_FILE to override)" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

echo "→ using $(basename "$ENV_FILE") (CDP_API_KEY_ID=${CDP_API_KEY_ID:+set}, CDP_NETWORK=${CDP_NETWORK:-base-sepolia})"

GOTOOLCHAIN=local GOPROXY=direct GOSUMDB=off GOFLAGS=-mod=mod \
  GOCACHE="$ROOT/_tools/gocache" GOPATH="$ROOT/_tools/gopath" \
  PAYGATE_LIVE=1 \
  "$GO" test ./payment/internal/cdp -run TestLiveCDPFullFlow -v -timeout 10m
