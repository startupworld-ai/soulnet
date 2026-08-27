# payment/ — SoulMirror 支付网关（paygate）

基于 Coinbase CDP v2 的本地 USDC 支付网关：一个随灵镜插件安装的**本地独立进程**，
监听 `127.0.0.1:9001`（仅回环），为 A2A 支付提供钱包创建、USDC 转账、链上入账验证。

架构与决策见 [`docs/cdp-a2a-payment-plan.md`](../docs/cdp-a2a-payment-plan.md)（§5 网关设计）。

## 为什么是本地进程

- **每人本地网关、自配 CDP**：没有公共服务器、没有平台托管；CDP 密钥各配各的，只存在本机
  keychain/环境变量，**代码仓库里永远没有真实密钥**（只有 `.env.example` 占位符）。
- **互通靠链不靠 CDP**：结算在 Base 链的 USDC 上，任何地址之间可直接转账。
- 三档能力：① 无 CDP（收钱/手动付款/验证）；② 本地 CDP（分身自动付款全功能）；
  ③ 未来公共网关（同一套代码，改 `gateway_url` 配置）。

## 构建

仓库 go.mod 要求 Go ≥ 1.25（本机 1.19 无法编译；可用仓库根的 `_tools/go`）。

```sh
go build -o bin/paygate ./payment/cmd/paygate
```

## 运行（开发，无密钥也可启动）

```sh
# 最小启动：只开 manual-address 档（无 CDP），join.verify / balance 可用
PAYGATE_HOME=$HOME/.soulmirror/a2a/pay ./bin/paygate
```

启用 CDP 全功能（环境变量，或在设置页录入）：

```sh
export CDP_API_KEY_ID=...
export CDP_API_KEY_SECRET=...     # base64 Ed25519(64B) 或 PEM EC P-256
export CDP_WALLET_SECRET=...      # base64 DER EC P-256 PKCS8
export CDP_NETWORK=base-sepolia   # 或 base
./bin/paygate
```

## 接口

全部请求须带 A2A 请求签名（`X-A2A-Pub` / `X-A2A-Timestamp` / `X-A2A-Signature`，
与 relay `VerifyRequest` 同格式，复用 `a2a.SignReq`）。

| 端点 | 说明 | 需要 CDP |
|---|---|---|
| `POST /v2/pay/wallet.create` | get-or-create 分身钱包（CDP EVM account，按指纹命名） | ✅ |
| `GET /v2/pay/wallet` | USDC/ETH 余额（CDP 或公开 RPC） | 收款地址即可 |
| `POST /v2/pay/transfer` | 分身转账 USDC（构造 EIP-1559 交易 → CDP 代签代发） | ✅ |
| `POST /v2/pay/join.verify` | 付费进群入账验证（公开 Base RPC 解析 Transfer 日志） | ❌ |
| `POST/GET /v2/pay/config` | 三档模式配置（密钥不经此接口） | — |
| `GET /v2/pay/health` | 健康检查 | — |

## 测试

```sh
# 单元测试（RLP 向量 / JWT 结构 / 链上日志解析 / 金额换算）——离线可跑
go test ./payment/...

# 端到端（真实 Base Sepolia RPC + 真实转账样本）
PAYGATE_LIVE=1 go test ./payment/internal/payapi -run TestLiveJoinVerify -v
```

## 目录

```
payment/
├── cmd/paygate/          入口（配置加载、HTTP 服务、优雅退出）
├── internal/cdp/         CDP v2 REST 客户端：平台 JWT / X-Wallet-Auth JWT、
│                         create account、token-balances、send/transaction、
│                         EIP-1559 RLP（USDC ERC-20 transfer）
├── internal/rpcclient/   公开 Base RPC：交易收据、Transfer 日志解析、余额、gas
├── internal/payapi/      /v2/pay/* HTTP 层 + A2A 验签中间件
├── internal/store/       wallets.json / transfers.jsonl / config.json
├── internal/config/      环境变量加载
└── .env.example          密钥占位符（永远不填真实值）
```
