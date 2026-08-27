#!/usr/bin/env bash
# Request testnet funds from the CDP faucet for an address.
#
# Usage (run in YOUR terminal, from the repo root):
#
#   bash payment/scripts/faucet.sh <address> <token> [count]
#
#   address  0x… wallet address to fund (e.g. your alter's USDC wallet)
#   token    usdc | eth | eurc | cbbtc   (default usdc)
#   count    how many faucet requests (default 1; usdc caps at 10/24h per address)
#
# Secrets are sourced from ~/.soulmirror/a2a/pay/.env locally (override with
# PAYGATE_ENV_FILE). The CDP API is reached through your proxy when
# https_proxy / PAYGATE_PROXY is set — required on networks that block Coinbase.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENV_FILE="${PAYGATE_ENV_FILE:-$HOME/.soulmirror/a2a/pay/.env}"
GO="$ROOT/_tools/go/bin/go"

if [ ! -x "$GO" ]; then
  echo "error: Go 1.25 toolchain not found at $GO" >&2
  exit 1
fi
if [ ! -f "$ENV_FILE" ]; then
  echo "error: env file not found at $ENV_FILE (set PAYGATE_ENV_FILE to override)" >&2
  exit 1
fi
if [ "$#" -lt 2 ]; then
  echo "usage: faucet.sh <address> <token> [count]" >&2
  exit 2
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a
if [ -n "${PAYGATE_PROXY:-}" ]; then
  export HTTPS_PROXY="$PAYGATE_PROXY" HTTP_PROXY="$PAYGATE_PROXY"
fi

export GOTOOLCHAIN=local GOPROXY=direct GOSUMDB=off GOFLAGS=-mod=mod \
  GOCACHE="$ROOT/_tools/gocache" GOPATH="$ROOT/_tools/gopath"

"$GO" run ./payment/cmd/faucet "$@"
