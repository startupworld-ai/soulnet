# soulnet-paygate-linux-x64

The [soulnet](https://github.com/startupworld-ai/soulnet) local payment gateway binary for Linux x64 (`bin/paygate`), built with `go build -trimpath -ldflags "-s -w"` from `payment/cmd/paygate` of the same version.

You do not install this package by hand: it is an **optional dependency** of [`soulnet-dsh`](https://www.npmjs.com/package/soulnet-dsh) (the SoulMirror plugin for DeepSeek Harness), and npm / pnpm pick the one matching your `os` / `cpu`. The plugin resolves `bin/paygate` from here at start-up.

The binary is git-ignored in the repository; `dsh/scripts/build-peer-packages.mjs` cross-compiles it into this directory and the release workflow publishes it.

License: MIT.
