# dsh/ — 灵镜 for DeepSeek Harness（`soulnet-dsh`）

[English](README.md) | 中文

一个 DeepSeek Harness（dsh）**组合包**：装进 dsh 的 web profile，这个 dsh 就成了
灵镜网络上的一个节点——A2A 身份、好友、端到端加密消息——**你的分身**住在一条普通的
dsh 会话（「我的分身」）里，替你和所有好友往来。聊天界面是一组 dsh 客户端插件
（slot）——dsh 侧栏右侧的一整页「灵镜」——不替换 dsh 的 UI。

状态：**M1——真实后端 + P2 侧边栏入口 + P4 对话模型：一条分身会话、好友只读往来、
主人审稿、dsh 风格页面**。网络后端是 `soulnet` 灵网轻端（Go，`../cmd/soulnet`、
`../peer`）：host 插件拉起这个二进制，经 stdio JSON-RPC 2.0 驱动它（协议见
`cmd/soulnet/README.md`，`soulnet/1`）。P0 spike 的内存假后端仍可选（`backend: fake`），
供测试与做界面用。spike 结论保留在 [SPIKE.md](SPIKE.md) 作为历史。目录结构、构建
规则、安装步骤见英文 README；这里是机制速记。

## 对话模型（P4）

灵镜的语义原样落进界面：**主人只和自己的分身说话**；分身替主人对所有好友开口；
**好友条目对主人只读**（分身对分身的往来）；分身想在没有主人吩咐的情况下发出的
话，变成一条**等主人拍板的拟稿**。

| 部件 | 机制 |
|---|---|
| **一条分身会话** | sessions 插件只创建一条 dsh 会话（标题 `My alter · SoulMirror`，预设 `soulmirror-chat`，cwd = `<home>/a2a`，P5 起不再挂自己的工作区），id 存 `<home>/a2a/dsh-sessions.json`（`{ alterSessionId }`），重启后 resume。它的 agent 持有面向**所有好友**的 `soulmirror_*` 工具（`soulmirror_send_message {fingerprint, body}` 等）。模型 = 创建时的 `ctx.agentDefaultModel.currentSelection()`。 |
| **「我的分身」= 唯一的输入框** | 灵镜页面中栏把**我的分身**钉在第一行（不参与排序）。右栏是我们自己从 `GET /soulmirror/api/session.history` 渲染的分身会话聊天视图（`chatFromEvents`：主人消息、分身便签、好友来信卡片、它发出 / 拟稿的内容、拟稿处理记录、失败的轮次）+ 输入框 → `POST alter.instruct {text}`（一条主人 `user/message`，`source: { kind: 'user' }` + `agent.followup()`）。SSE `alter` 帧触发重取（防抖）。**在 dsh 里打开**跳到原生会话（轨迹 / 工具），它的原生输入栏同样可用。 |
| **好友条目 = 只读往来** | A2A 归档（`conversation.get` + SSE `message` / `outbound`）：左 = 对方（或其分身），右 = 我的分身，带送达状态、日期分隔、上翻加载；顶部横幅「这是你的分身与 X 的分身的往来，你不能直接发言；要说话去找「我的分身」」；**没有输入框**；底部是**操作条**：待确认 n · 名片 · 准则 / 好友设置（档位 + 按好友补充）· 去找「我的分身」说话。页面可见时选中好友即标已读。 |
| **来信 → 分身会话** | 每位好友的来信都作为一条 relay `user/message` 追加进**分身会话**（`source: { kind:'plugin', plugin:'soulmirror', form:'relay', senderSessionId:<好友名>, a2a:{ id, fp, ts, auto?, type? } }`，带好友上下文），按该好友的档位路由（`routeInbound`）：`notify` → 只追加；`draft` / `auto` → `agent.followup` 唤醒一轮。防环：带 `auto` 标的来信和非好友来信永不唤醒轮次。分身用 `soulmirror_send_message` + 来信人的指纹回复。 |
| **按轮次解析的好友上下文** | 人设（`persona.ts`，agent 作用域的 `deployment:persona` 段 + 提示变量，在 agent 工厂的 `setup` 里注册，创建和恢复都走）带主人名、**花名册**（每位好友：名字 · 指纹 · 档位 · 准则补充）、全局准则、待拍板拟稿，以及**这一轮的好友**——组装时从会话日志读回（`triggerOf`）：唤醒这一轮的来信是谁发的（+ 其档位与补充），主人吩咐则为 "none"（主人可以点名任何好友，指纹在花名册里）。 |
| **发送门 → 直发或拟稿**（`sendGate`） | 主人吩咐的一轮 → 对任何好友**直发**（不打 auto）。来信人本人、`auto` 档、未超 `autoReplyPerHour` → **直发并打 `auto` 标**（`message.send {auto:true}`），按好友计数（`HourlyWindow`）。其余一律 → **`queueDraft`**：draft / notify 档、超限、被自动来信唤醒的一轮、回复 A 却写给 B、无法归因的触发。工具结果 `outcome: 'draft-queued'` + `draftId`；人设要求分身一句话告诉主人、不要重试。这条路径**不再走 dsh 审批面板**（只在没有 sessions 服务时作兜底，以及 `soulmirror_add_friend`）。 |
| **拟稿归我们管** | `<home>/a2a/dsh-pending.json`（`drafts.ts`）：`{ id, fp, name, body, createdAt, reason, trigger, sessionId }`。页面在好友只读往来里和「我的分身」聊天里渲染拟稿卡（`DraftCard.tsx`，原型 `.wx-pending`）：**允许发送** → 宿主经轻端以分身身份发出（`auto:false`）、归档（SSE `outbound`）、删掉拟稿；**改一改** → 文本框、按改后的发；**让分身改** → 意见框 → 丢弃拟稿，给分身一条带意见的主人吩咐（它重写后按主人的话直发）；**拒绝** → 丢弃。每个决定同时作为插件 NOTE 写进分身会话（relay 来源 + `a2a.note`，永不触发轮次），分身下次运行就知道结果。`POST drafts.decide {id, action, body?, feedback?}`、`GET drafts.list`；SSE `draft` 帧（`added` / `removed`）让每个标签页实时更新；`/state` 好友行带 `drafts`（数量），表头显示总数。 |
| **中栏**（原型 #B） | 表头（身份名、指纹、复制名片、未读 + 拟稿角标、准则编辑、刷新、关闭）· 搜索 · **我的分身**钉在第一行（在线点、「n 项等你拍板」）· **新的朋友**（通过 / 忽略）· **我的好友**（头像带在线点和未读角标、档位胶囊、最近一条 + 时间——有拟稿时前缀 `[待确认]`；未读优先、再按最近）· 底部**加好友**。左边缘 = dsh 侧栏右边缘（量我们自己 footer 按钮所在列的 DOM；ResizeObserver + 收起过渡期间轮询；rail 时 56px）。 |
| **样式** | 只用 dsh 的 token（ui-layout / ui-theme 设在文档上的 `--dsw-alias-*` / `--dsw-specific-*`）+ ui-primitives（`Button`、`Tooltip`、图标、`Toast`），一个注入的 `<style>`；布局与行为照灵镜原型，配色与字体照 dsh（主人拍板）。 |
| **从 P3 迁移** | P3 的按好友会话退役：旧 `dsh-sessions.json` 的 `{ sessions: { fp: id } }` 保留为 `legacyFriendSessions`（记一次日志，设置里显示）；那些会话仍留在 dsh 侧栏直到删除，但不再收信，也不再创建新的按好友会话。从这类会话发起的工具调用与其他会话同等对待（看唤醒那一轮的是谁）。 |
| **保留** | 侧栏底部入口的未读角标 + 新来信 toast（好友往来正开在页面上、或分身会话正在屏幕上时不弹）、SSE 实时更新、设置 → 灵镜网络（准则编辑、默认档位、每小时上限、调试**以我本人发送**开关——打开后好友往来里出现一个绕过分身的直发小栏；默认关）、斜杠命令（`/card`、`/friends` → 弹出选择打开页面到某好友、`/add`、`/soulmirror` → 页面 / 好友）、中英文、首次引导。P2 的「灵镜」`conversation.view` 标签在 P5 去掉了（页面 + 侧栏入口就是入口）；分身会话不再挂自己的工作区（P5），在会话列表里是未分组的一行。P3 的好友会话 composer 接管随好友会话一起去掉。 |

宿主 API（P4 变化）：`POST alter.instruct {text}`；`GET session.latest` →
`{state: {sessionId, status, latest}}`；`GET session.history?limit=` →
`{sessionId, status, chat: {items, running, seq}}`；`GET drafts.list?fp=`；
`POST drafts.decide {id, action: approve|reject|revise, body?, feedback?}`；
`/state` → `drafts[]`、好友行带 `tier` / `tierExplicit` / `protocol` / `drafts`、
`alter: {sessionId, status, defaultTier, autoReplyPerHour, directSend, protocolPath,
protocolExists, legacyFriendSessions}`；SSE 帧 `alter`、`outbound`、`draft`；
`friends.open` 与 `sessions` 映射已去掉。**宿主改了 → 更新后要重启 dsh。**

测试：`test/policy.test.ts`（档位路由、防环、发送门 → 直发 / 拟稿、限频窗口）、
`test/alter-state.test.ts`（带好友的唤醒归因、带发送结果的折叠、聊天记录折叠）、
`test/drafts.test.ts`（存储）、`test/tools-gate.test.ts`（真实工具对假上下文：主人对任何
好友直发、auto 档 + 上限 + auto 标、draft 档 → 存拟稿且不审批、notify / 防环 / 别的好友 /
未知触发 → 拟稿、没有 sessions 服务时的审批兜底）、`test/inbox-state.test.ts`（draft 帧、
按行的拟稿计数）、`test/page-state.test.ts`（钉住的「我的分身」选择）、
`test/friend-settings.test.ts`、`test/soulnet-client.test.ts`、Go `cmd/soulnet/rpc_test.go`。

真实运行（`spike-evidence/p4-real-run.txt`、`spike-evidence/p4-*.png`）：dsh 跑 3096 端口、
独立 `DSH_HOME`、本地 relay、另外两个 `soulnet` 轻端（Bob 在 `draft` 档、Carol 在 `auto` 档），
以及——本机没有模型 key——一个挂在 `llm-deepseek` `baseURL` 上的**脚本化 OpenAI 兼容服务**，
按系统提示里的花名册扮演分身。展示了：在「我的分身」里吩咐 → 分身直发给 Bob（`owner-initiated`）
→ Bob 的只读往来里出现「发」气泡；Bob 在 `draft` 档来信 → Bob 往来里出现拟稿卡（中栏
`[待确认]` 标记 / 表头计数）→ **允许发送**送达 Bob、**拒绝**什么都不发、**改一改**送达改后的
文本、**让分身改**让分身重写后按主人的话发出；「我的分身」聊天里有来信卡、发送行
（已发出 / 拟稿）、处理记录；Carol 在 `auto` 档收到带 `auto: true` 的自动回复；带 auto 标的来信
只追加不起轮次；借钱 → 不发送、给主人便签；`notify` → 只追加；侧栏收起（left = 56）；在 dsh 里打开。

## 侧边栏入口（P2）

`sidebar.footer.action`（wide：图标 + 文字 + 未读胶囊；rail：图标 + 红点）——点击开关
灵镜页面；新来信 toast（ui-primitives
`Toast`，4 秒，不可点）；`src/client/inbox-state.ts` 把最近一次 `/state` 与 SSE 帧折叠
（`message` / `presence` / `typing` / `friend_accept` / `friend_request` / `outbound` / `draft`），
角标与行即时变化，随后防抖重取覆盖乐观折叠。向 dsh 上游提的需求：「新会话」按钮旁没有
可追加的槽位（`sidebar.top.action`）。

## 历史：P2b 页面与 P3 按好友会话

P2b（`spike-evidence/p2b-*.png`）把页面做成好友列表 + 聊天面，输入框**直发**给好友；
P3（`spike-evidence/p3-real-run.txt`）把输入框改成对分身的吩咐，但保留了**每位好友一条
dsh 会话**（该好友的分身）、按档位路由来信、`draft` 档走 **dsh 审批面板**、还有一条
「分身便签条」。主人看过界面后否了这个对话模型：主人只能和唯一的分身说话，好友只读，
拟稿在页面上审。P4（本次）就是这次纠正；按好友会话、好友会话 composer 接管、便签条、
审批面板路径都去掉了。活下来的底层机制：relay `user/message`（不造自定义会话事件，
SPIKE.md §1）、从日志读回触发的发送门、带实时变量的 agent 作用域人设、档位 + 限频 + 防环、
线上的 `auto` 标、轻端的 `friends.card` / `friends.set {protocol}`。

## 构建与测试

```sh
go build -o bin/soulnet ./cmd/soulnet            # Windows: -o bin/soulnet.exe
cd dsh && pnpm install && pnpm run build
pnpm run typecheck                               # host / client / test 三份

cd packages/dsh
pnpm test:unit                                   # JSON-RPC、soulnet 客户端、policy、alter-state、drafts、tools gate、inbox / page state
pnpm test:integration                            # 真 soulnet + 本地 soulmirror-relay（两个身份两个 home）
```

集成测试需要仓库根的 `bin/soulnet[.exe]` 与 `bin/soulmirror-relay[.exe]`
（或用 `SOULNET_BIN` / `SOULMIRROR_RELAY_BIN` 指定）。缺二进制时**跳过**；
`SOULNET_INTEGRATION=1` 使其变为失败，`SOULNET_INTEGRATION_VERBOSE=1` 打印日志。
**绝不指向公共 relay**。

## 安装进 dsh 并运行

### 普通用户（已发布到 npm，一行搞定）

不需要 Go、不需要 clone。有 Node >= 22.19 和 pnpm（`npm i -g pnpm`）即可：

```sh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add soulnet-dsh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3099     # 打开 http://127.0.0.1:3099/
```

`dsh plugin add` 会在 `$DSH_HOME/profiles/web` 里 `pnpm add`：装上 `soulnet-dsh`，并只装与本机匹配的那一个
平台二进制包 `soulnet-peer-<os>-<arch>`（`windows-x64` / `darwin-arm64` / `darwin-x64` / `linux-x64` / `linux-arm64`）。
卸载：`dsh plugin --profile web remove soulnet-dsh`。

### 开发者（从 checkout 链接）

```sh
export DSH_HOME=/tmp/dsh-home            # PowerShell: $env:DSH_HOME = "C:\tmp\dsh-home"
cp bin/soulnet dsh/packages/dsh/bin/     # 让二进制可被找到（或放 PATH）；Windows: soulnet.exe

npx -y @deepseek-ai/dsh@0.1.1-rc.2 plugin --profile web add ./packages/dsh
npx -y @deepseek-ai/dsh@0.1.1-rc.2 --profile web --dump-config | grep -n soulmirror
npx -y @deepseek-ai/dsh@0.1.1-rc.2 web --no-open --port 3099
```

打开 `http://127.0.0.1:3099/`：首次引导起昵称、建身份、给出名片链接与备份提醒；或在
设置 → 灵镜网络 里做。点侧栏底部的 **灵镜**：页面开在**我的分身**上——吩咐它
（「告诉 Bob 我三点到」），它经 `soulmirror_send_message` 写给好友，左边好友的往来里出现
「发」气泡。加好友：在列表底部的「加好友」贴名片链接（或 `/add <card_uri>`），或在
「新的朋友」里通过。好友条目是你的分身与对方分身的只读往来。`draft` 档（默认）的好友来信
会唤醒分身，它想回的话以**拟稿卡**出现在该好友的往来和「我的分身」里——允许发送 / 改一改 /
让分身改 / 拒绝。`auto` 档自己发（限频、打 `auto` 标）；`notify` 只展示来信。

按 profile 覆盖写进 `$DSH_HOME/profiles/web/cordis.patch.yml`（同名字段也是用户设置）：

```yaml
- id: soulmirror-network
  config:
    backend: soulnet            # soulnet | fake
    relay: http://127.0.0.1:9390   # 测试用本地 relay；默认 https://relay.soulnet.startupworld.cn
    home: C:\tmp\soulnet-home
```

卸载：`dsh plugin --profile web remove soulnet-dsh`。

## 运行期文件

`<home>/a2a/`（默认 `~/.soulnet/a2a/`）：identity.json、friends.yaml、conversations/ …（与
`~/.soulmirror/a2a/` 同布局）；`dsh-sessions.json` = `{ alterSessionId, legacyFriendSessions? }`；
`dsh-friends.json` = 按好友档位；`dsh-pending.json` = 分身的待拍板拟稿；`protocol.md` = 外交准则；
`dsh-sessions.log` = 插件自己的日志。`$DSH_HOME/.agent-presets/soulmirror-chat/` = 预设
（首次运行拷入）；`$DSH_HOME/sessions/...` = dsh 自己的会话日志。

## 路线图

P0 spike → **M1 网络与身份** → **P2 侧边栏入口、未读角标、toast** → P2b 聊天面 → P3 让分身替你说话
→ **P4 灵镜对话模型：一条分身会话（「我的分身」钉第一、唯一输入框）、好友只读往来 + 操作条、
主人审稿（允许 / 改一改 / 让分身改 / 拒绝）取代审批面板、人设里按轮次的好友上下文、dsh 风格页面
（本次）** → 接下来：在原生分身会话里遮蔽「上下文注入」行、composer 附件、好友往来里的任务卡 /
状态条、界面上按好友的自动回复计数、`/protocol` 命令、首轮 nudge 让分身会话在 dsh 自己的列表里
永不 blank。待 dsh 上游：插件会话事件注册 / append 时 `ignorable`；侧边栏未读角标或非 blank 标记；
「新会话」按钮附近的 `sidebar.top.action` 槽位；会话置顶 API；blank 会话行应尊重已投影的标题；
外部插件的客户端 Remote 注册面。
