#!/usr/bin/env bash
# SoulMirror (DSH edition) macOS launcher -- the CFBundleExecutable of
# SoulMirror.app. Boots the DeepSeek Harness web profile with the bundled
# Node.js runtime, with the soulnet-dsh plugin pre-installed.
#
# Where things live:
#   $DSH_HOME               dsh home (profiles, sessions, settings).
#                           Defaults to ~/.dsh-soulmirror (dedicated -- an
#                           existing ~/.dsh of another dsh setup is untouched).
#   ~/.soulnet              the soulnet identity / friends / conversations.
# Neither directory is removed on uninstall (delete the .app to uninstall).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../Resources" && pwd)"

if [[ -z "${DSH_HOME:-}" ]]; then
  DSH_HOME="$HOME/.dsh-soulmirror"
fi

# Pick the portable Node runtime for this Mac.
case "$(uname -m)" in
  arm64) NODE_DIR="$ROOT/node-arm64" ;;
  *)     NODE_DIR="$ROOT/node-x64" ;;
esac

export PATH="$NODE_DIR/bin:$ROOT/app/node_modules/.bin:$PATH"
TEMPLATE="$ROOT/home-template"
PROFILE="$DSH_HOME/profiles/web"
MARK=".soulmirror-template-version"

# First run: seed the dsh home with the pre-installed web profile (offline).
if [[ ! -f "$PROFILE/package.json" ]]; then
  echo "First run: preparing the dsh profile at \"$DSH_HOME\" ..."
  mkdir -p "$DSH_HOME"
  cp -R "$TEMPLATE/." "$DSH_HOME/"
fi

# Upgrade: when the installed template is newer, refresh ONLY the packages we
# ship (never the user's own plugins or settings).
if ! cmp -s "$TEMPLATE/profiles/web/$MARK" "$PROFILE/$MARK"; then
  echo "Updating the SoulMirror plugin in \"$PROFILE\" ..."
  for P in soulnet-dsh soulnet-peer-darwin-x64 soulnet-peer-darwin-arm64 soulnet-dsh-sidebar; do
    if [[ -d "$TEMPLATE/profiles/web/node_modules/$P" ]]; then
      rm -rf "$PROFILE/node_modules/$P"
      cp -R "$TEMPLATE/profiles/web/node_modules/$P" "$PROFILE/node_modules/$P"
    fi
  done
  cp "$TEMPLATE/profiles/web/$MARK" "$PROFILE/$MARK"
fi

exec "$NODE_DIR/bin/node" "$ROOT/app/node_modules/@deepseek-ai/dsh/lib/bin.js" web "$@"
