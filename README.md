<div align="center">
  <h1>API Key 团队管理</h1>
  <p><strong><a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI</a> 下游 API Key 管理与订阅额度插件。</strong></p>
  <p>
    <a href="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/releases/latest"><img src="https://img.shields.io/github/v/release/CHIMOOO/cpa-plugin-key-billing-manager?label=release" alt="Latest release"></a>
    <a href="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/actions/workflows/check.yml"><img src="https://github.com/CHIMOOO/cpa-plugin-key-billing-manager/actions/workflows/check.yml/badge.svg" alt="CI status"></a>
    <img src="https://img.shields.io/badge/platforms-Windows%20%7C%20macOS%20%7C%20Linux-blue" alt="Platforms: Windows, macOS, and Linux">
    <a href="./LICENSE"><img src="https://img.shields.io/badge/license-MIT-green" alt="MIT License"></a>
  </p>
  <p><strong>简体中文</strong> · <a href="./README.en.md">English</a></p>
</div>
<img src="images/example.png" alt="cpa-plugin-key-billing example" width="100%" />

## 功能特性

- 支持金额、Token、请求三种额度，可按 API Key 独立计时或按订阅计划统一周期重置
- 独立「订阅计划」页支持按指定模型、多个模型或模型类别配置额度池；每个 Key 的各池分别累计用量
- 支持按输入 Token 阈值切换长上下文**阶梯计价**
- 支持按 API Key 设置**最大并发请求数**
- 支持为每个 API Key 绑定**路由规则**，限制模型访问范围和上游凭证
- 支持在独立的「API Key 分组」页中管理分组；API Key 可加入多个分组，按组绑定路由规则或直接选择上游凭证、模型，批量设置成员与凭证白名单
- 支持全局访问控制开关，以及可选的未分组 Key 默认拒绝策略
- 支持分组独立启停，禁用后保留成员与规则
- 独立「账号接入」页支持 OpenCode Go / Zen、CommandCode、Cline Pass，查询可用订阅额度并发布到 CPA 上游配置
- 独立「账号管理」页展示逐账号用量与当前并发，支持逐账号设置并发上限；分组内账号标签展示实际返回的 5 小时、周额度
- 「风控中心」支持本地关键词、模型范围、观察与请求前拦截，以及命中内容的哈希记忆
- 集成 Codex turn-state 模板采集与注入，支持在独立的「Codex Turn State」页中切换 `replace-only` / `always` 和配置探测代理
- 可从 [models.dev](https://models.dev/) 获取模型参考价
- 支持简体中文与英文；独立页面可切换并记住语言，嵌入管理中心时跟随宿主语言

## 工作原理

插件会在请求到达上游前检查订阅额度、并发和路由。上游调用结束后，CLIProxyAPI 通过 `usage.handle` 提供用量。插件据此记录请求事件、计算费用并更新周期消费额。

```mermaid
---
config:
  themeVariables:
    fontSize: "13px"
  flowchart:
    diagramPadding: 4
    nodeSpacing: 20
    rankSpacing: 28
    padding: 3
---
flowchart TB
    A[下游请求] --> B["request.intercept_before<br/>模型 · 并发 · 额度"]
    B -- 拒绝 --> R[返回 HTTP 403 / 429]
    B -- 通过并占用并发槽 --> C["scheduler.pick<br/>选择路由允许的上游凭证"]
    C -- 无可用凭证 --> S[返回 HTTP 503]
    C -- 成功 --> D[CLIProxyAPI 调用上游模型]
    D --> E["request.complete<br/>释放并发槽位"]
    D --> F["usage.handle<br/>记录请求事件与用量"]
    F --> G["归一化 Token 并计费<br/>更新周期消费额"]
```

插件以同步 RPC 方法接入 CLIProxyAPI。请求准入与凭证调度执行本地规则计算，用量记录与计费只依赖 `usage.handle`。启用 turn-state 后，会观察上游响应头并同步保存有效模板，不解析响应正文或据此重建计费。插件不创建后台协程、定时器或异步刷新任务；代理探测由管理页面逐次触发。

## 环境要求

- CLIProxyAPI `7.2.143` 或更高版本，建议使用最新版本
- Codex turn-state 的 HTTP/SSE 注入需要官方 `7.3.4` 或包含对应请求头转发修复的版本；`7.2.143` 的执行器会丢弃该头，插件显示注入决策也不能证明上游实际收到
- 使用支持插件的 CLIProxyAPI 构建，不要使用 no-plugin 版本

## 安装

本分支新增功能可按下文「在 CPA 中调试本地修改」编译加载，或通过本仓库的 `registry.json` 安装本仓库发布的构建。以下安装脚本下载本仓库的发行版。

在 CLIProxyAPI 根目录运行。macOS 和 Linux 使用：

```sh
curl -LsSf https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.sh | sh
```

Windows 请先停止 CLIProxyAPI，再在 PowerShell 中运行：

```powershell
irm https://raw.githubusercontent.com/CHIMOOO/cpa-plugin-key-billing-manager/main/install.ps1 | iex
```

安装脚本会将插件安装到当前目录的 `plugins/`。安装或升级完成后需要重启 CLIProxyAPI。

也可以从 [Releases](../../releases/latest) 下载对应平台的发布包，解压后将动态库放入 CLIProxyAPI 的 `plugins/` 目录：

```text
plugins/cpa-team-manager.so       # Linux
plugins/cpa-team-manager.dylib    # macOS
plugins/cpa-team-manager.dll      # Windows
```

插件商店显示名称为「API Key 团队管理」，可搜索 `api-key`、`团队`、`分组`、`额度`、`订阅`、`计费`、`路由`、`codex`、`turn-state` 等词。从 v0.0.8 起，插件 ID、配置键、动态库文件名及访问地址改为独立的 `cpa-team-manager`，避免与原项目冲突。

### 从 v0.0.7 及更早的本项目版本升级

1. 停止 CPA，备份配置、原 `state_file` 数据库及其旁边的 `.turn-state.json` 文件。
2. 将本项目旧的 `cpa-key-billing.so` / `.dylib` / `.dll` 移出插件加载目录；不要同时启用两份计费与调度插件。
3. 在 `plugins.configs` 中将本项目的配置键改为 `cpa-team-manager`，**保留原 `state_file` 的完整路径**，例如 `plugins/cpa-key-billing-state-v1.db`。新 ID 默认使用新的数据库路径，省略此项不会自动接管旧数据。
4. 安装新版动态库，启动 CPA，打开下文的新地址并核对原分组、订阅和桶。账号指纹保持兼容，已有绑定无需重选。

新安装使用下方配置。原项目与本项目必须使用不同的数据文件；不要让两个插件同时打开同一数据库。回退时停止 CPA、恢复备份与旧插件配置。

## 配置

在 CLIProxyAPI 配置文件中加入：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-team-manager:
      enabled: true
      debug: false # 是否记录 debug 日志，例如路由日志、匹配参考价日志
      codex_fast_mode_billing: false # 开启后，Codex 的 priority 请求按 2.5 倍计费
      state_file: "plugins/cpa-team-manager-state-v1.db"
```

`codex_fast_mode_billing` 开启后，请求 Codex 上游时在请求中指定 `service_tier=priority`，按普通费用的 **2.5 倍**结算。

> [!WARNING]
> 升级前请备份数据文件。
>
> - v0.0.7 将计费数据库升级到 v19，保存模型范围额度语义；升级后请勿用旧版插件直接打开该数据库。回退时应恢复升级前的备份，同时保留旁边的 `.turn-state.json` 文件。
> - v1.0.0 至最新版本的数据库文件支持自动迁移。
> - v0.8.4 及更早版本的 JSON 或 SQLite 数据文件不支持迁移，请将 `state_file` 指向新文件。

重启 CLIProxyAPI 后，在管理中心打开「API Key 团队管理」。确认模型定价后，创建订阅计划并绑定需要限制的 API Key。

## 页面访问

管理员可以从 CLIProxyAPI 管理中心的「API Key 团队管理」菜单进入，也可以直接打开：

```text
http(s)://<CLIProxyAPI 地址>/v0/resource/plugins/cpa-team-manager/ui
```

普通用户使用自己的 API Key 查询订阅额度和用量时，直接打开：

```text
http(s)://<CLIProxyAPI 地址>/v0/resource/plugins/cpa-team-manager/ui#account
```

### 持久化提示

「设置」页会检查可识别的 Linux 容器部署中，插件库、计费数据库和插件数据文件所在的挂载位置。仅检测到容器可写层、内存文件系统或已加载但磁盘缺失的插件库等风险时显示提示及需处理的路径；已检测到正常外部挂载的状态不展示，其他页面不展示此模块。容器重建可能丢失可写层文件；这与普通 CPA 进程重启不同。插件库或数据丢失后，分组与额度限制可能不再生效。

请备份数据，为插件和数据目录配置持久挂载，并在部署配置中保留插件加载项。CPA 没有向插件开放安全修改容器挂载的能力，因此这里提供具体提示，不提供无法保证生效的“一键设置”。检测到外部挂载只证明当前路径的挂载状态，不保证自动加载、未来部署或备份正确；无法检测的部署不显示推断结论。

## 计费与订阅规则

- 未绑定订阅计划的 API Key 只统计用量，不限制额度。
- 订阅计划可设置多个自定义额度窗口，每个窗口可单独或组合限制金额、Token、请求数。
- 每个额度池可覆盖全部模型、指定的一个或多个模型，或 OpenAI、Claude、xAI、Gemini、DeepSeek、Qwen 模型类别。多个模型放在同一池时共用该池额度；想分别限制时创建不同的池，重置周期可以相同。例如 `gpt-6-astra` 每天 100 美元、`gpt-5.5` 每天 200 美元，互不占用余额。
- 类别按请求中的模型名称识别，例如 OpenAI 对应 GPT/o 系列、xAI 对应 Grok；CPA 的兼容接口名称不是模型类别。任意自定义别名应使用完整模型 ID 配置指定模型额度。未命中这些范围的模型只受「全部模型」额度及路由限制；没有全部模型池时，不受其他模型池的限制。
- 一个请求命中多个池时，各池同时累计和限制，例如同时设置全部模型 500 美元与 Claude 200 美元。费用记录仍只记一次；Claude 额度用尽不会阻止其他未耗尽的模型池。修改范围会重置该池用量，单纯调整限额保留已用量。
- 含模型范围额度池的计划要求请求指定具体模型。动态 `auto`（包括思考后缀）在 CPA 的准入与用量记录中可能对应不同模型，无法可靠归属时会返回 `503 quota_model_unresolved`，避免漏扣额度；只有全部模型额度池的计划继续支持 `auto`。
- 每个 API Key 独立记账。独立周期从首次放行开始；统一周期可为各窗口指定下次开始时间，所有绑定 Key 按固定时间重置。
- 手动重置额度时，统一周期的重置时间保持不变；独立周期在下一次放行时重新开始。
- 自定义价优先于 models.dev 参考价；两者都没有时正常放行，记录用量但费用为零。Token 和请求次数额度仍然生效，未定价用量不会扣除金额额度。
- 请求事件保留最近 365 天。

## 路由规则

在 API Key 页面绑定路由规则，也可在「API Key 分组」页通过分组授权：创建分组时可以绑定路由规则，也可以不建路由、直接选择允许的认证文件、配置凭证或模型，两者可同时使用；再把 Key 加入一个或多个组。API Key 可批量设置多个分组，分组成员列表支持搜索和按现有分组筛选。

点击模型或凭证的选框，可在未选择、白名单（勾号）、黑名单（叉号）之间切换。分组绑定的路由、分组的直接选择、Key 直接绑定的路由和 Key 的直接选择共同合并：白名单取并集，黑名单取并集，黑名单优先。模型白名单为空时不限制模型；**受路由或分组管理的 Key，凭证白名单为空时不允许任何上游**。只有选中的凭证可以使用。保存 Key 的直接路由配置后，即使清空所有选项，仍表示禁止上游访问。选择整类凭证会授权该类别当前和未来新增的凭证；只允许指定认证文件时，应逐个选择文件，不选择整类凭证。既未绑定路由规则、也未直接选择凭证或模型的空组不授予权限；只有 Key 所属的全部分组都是空组时，请求才会返回“API Key 所属分组尚未绑定路由规则或上游凭证，访问已被禁止”。

「API Key 分组」页中的**启用访问控制**默认开启，保存后刷新页面或重启 CPA 都会保留。关闭后暂停分组、模型与凭证访问限制，CPA 自身认证及插件的计费、额度和并发限制仍然生效。**拒绝所有未分组的apikey**默认关闭；开启并保存后，所有未分组 Key（包括后续新建的 Key）都不能访问模型。关闭时，没有任何分组或路由限制的 Key 可使用 CPA 提供的上游。

每个分组都有独立启用开关。禁用后保留成员与规则，但不参与模型、凭证的授权或拒绝；Key 只有禁用组时拒绝访问，同时属于其他启用组时按这些组继续授权。升级后的已有分组默认启用。

同一个 Key 属于多个启用组时，先合并这些组的授权，再从 CPA 对当前模型提供的候选中选择可用账号。某个组绑定了停用账号，不会阻止另一个组的可用候选被选择；账号重新启用后按宿主的最新状态恢复参与调度。分组授权不会重新启用账号，也不会绕过零权重、模型冷却或其他组的黑名单。没有可用且获授权的候选时拒绝请求，不会退到未授权账号。

原 X-Forwarded-For 拦截功能和管理接口已移除；旧数据库中的设置仅为兼容保留，不再影响请求。

凭证选择器默认显示全部，Codex、xAI 分类包含对应服务商的配置凭证及 OAuth 认证文件；OAuth 按认证文件来源筛选。搜索和分类只改变显示范围，不清除已选项。批量选择仅作用于当前显示的单个凭证；需要授权未来新增凭证时，单独选择“整类”。API Key 只显示脱敏值：足够长时显示首尾各 8 个字符，短 Key 按长度缩减，4 个字符只显示首尾各 1 个字符。

升级前已有的仅模型或仅拒绝列表路由也遵循凭证白名单规则，需明确选择允许凭证后才能继续访问。

插件在调度时筛选凭证，并在 CPA 选出最终凭证后核对白名单。CPA 提供的候选范围仍受其优先级设置影响；若其他调度模式跳过插件调度，最终检查会拒绝未授权凭证，不会自行改选。按整类授权时，无法从宿主提供的目录确认凭证类别会拒绝访问，可改为明确选择单个凭证。

```mermaid
---
config:
  themeVariables:
    fontSize: "13px"
  flowchart:
    diagramPadding: 4
    nodeSpacing: 20
    rankSpacing: 28
    padding: 3
---
flowchart TB
    A["分组路由 ＋ 分组直接选择<br/>＋ Key 绑定路由 ＋ Key 直接选择"] --> M["模型黑白名单分别取并集<br/>白名单为空则不限，再排除黑名单"]
    A --> C["凭证黑白名单分别取并集<br/>整类凭证 ＋ 单个凭证<br/>白名单为空则拒绝，再排除黑名单"]
    M --> D{请求模型是否允许？}
    D -- 否 --> R[返回 HTTP 403]
    D -- 是 --> P["在 CPA 可用候选中<br/>按凭证权限选择上游"]
    C --> P
    P -- 有可用凭证 --> U[调用上游]
    P -- 无可用凭证 --> S[返回 HTTP 503]
```

## 账号接入、用量与并发

「账号接入」支持 OpenCode Go / Zen、CommandCode API Key 和 Cline Pass。Cline 可通过设备授权登录，或填写已有凭证；登录与刷新由当前页面发起。凭证保存在 CPA 的认证文件中，插件数据库不保存这些明文凭证。请同时持久化并备份 CPA 的 `auth-dir` 和上游配置。

保存账号后，将选中的模型发布到 CPA 上游。OpenCode 按已验证的模型协议自动生成 Chat Completions、Responses 和 Anthropic 通道；原生通道的客户端模型名带独立前缀，页面可复制。当前宿主无法配置 OpenCode Google 模型所需的接口路径，目录中未确认协议的模型也不发布；界面会列出原因。发布和删除只更新该账号所属通道，失败时保留重试入口；多类通道分别写入，部分成功会明确显示。

替换密钥或手动刷新 Cline 凭证时，先迁移准确的分组、路由和 Key 凭证引用，并让新旧凭证共用并发上限，再发布上游配置。全部通道确认更新后移除旧引用。操作中断时使用「连接 / 修复通道」继续，重试不会再次轮换凭证；待迁移记录保存在 CPA 认证目录中。取消设备登录会停止后续通道写入；若账号已保存，页面会明确告知，可在账号卡片中删除。

若上游已轮换凭证而 CPA 认证目录写入失败，新凭证只暂存在进程内存中；应先恢复存储并修复通道。此时若进程退出，可能需要重新登录。

| 接入类型 | 额度来源与显示 |
| --- | --- |
| OpenCode Go | 通过工作区 ID 和控制台 auth cookie 读取服务端订阅用量窗口 |
| OpenCode Zen | 可接入和发布模型；当前不查询预付费余额 |
| CommandCode | 官方 CLI 使用的组织订阅额度与余额接口 |
| Cline Pass | 官方订阅用量接口；仅发布该订阅可用的模型 |

额度按需刷新，只显示上游实际返回的窗口、比例或余额。未返回的 5 小时/周额度不补造，查询失败不显示为满额。分组中的账号标签通过精确凭证引用关联额度，不按名称或相似模型猜测；未确认的关联不展示订阅额度。上游订阅额度和本插件的下游订阅限额分别计算。

「账号管理」按宿主明确提供的账号索引汇总最近 365 天的 `usage.handle` 记录，展示请求数、成功/失败、可归类 Token、费用和并发。旧记录缺失的 Token 分类不能补回，会标记不完整记录。部分 CPA 版本不在认证清单中列出配置型 API Key：这些账号仍可预先设置并发，但用量需要首次请求确认准确身份；重启后可能需再次确认，不会将未知用量显示为零。

逐账号并发上限为 0–1000，0 表示不限。它与下游 API Key 并发同时生效：流式请求持续占位，结束、断开或切换重试账号时释放原账号槽位。设置保存在 `<state_file>.account-runtime.json`，备份时一并保留。

## 风控中心

风控默认关闭，支持按模型范围检查本地关键词。观察模式只记录命中；请求前拦截模式在调用上游前拒绝命中内容。拦截模式下无法检查的请求正文也会拒绝，包括缺失、格式不支持或超过检查上限的正文；当前检查最多 1 MiB JSON 和 64 KiB 可识别文本，不分析图片、音频或外部链接内容。

可记住命中内容的哈希，之后相同规范化文本也按策略处理。事件只保存时间、模型、规则与调用方引用，不保存请求正文、摘录或凭证。事件默认保留 30 天、最多 500 条，计数反映当前保留的事件；哈希记忆可单独清除。配置、事件和哈希保存在 `<state_file>.risk-control.json`。该功能是本地规则检查，不提供外部 AI 内容审核或对上游账号封禁风险的保证。

## Codex Turn State

在独立的「Codex Turn State」页中配置。该模块源自 [arden-aaai/cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state)（MIT License，Copyright © 2026 boooot），在其账号/模型模板算法、注入模式和代理重试规则基础上适配本插件的同步调用与独立状态存储。原项目出处、参考版本及完整许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。默认关闭；启用后设置即时生效，修改模式不需要重启 CPA。替换插件动态库仍需重启。

HTTP/SSE 业务注入要求 CPA `7.3.4` 或具有相同修复的版本。旧版 `7.2.143` 的 Codex 执行器不转发该请求头；设置开关和插件注入计数不能检测这个宿主限制。升级宿主后再验证实际请求行为。

**加入采集列表的账号默认启用业务保护**：只允许使用该账号、实际模型对应的有效模板。桶缺失或过期、注入关闭、观察模式以及 `replace-only` 无法替换当前请求时，均拒绝业务流量；其他账号仍按自身规则调度。该保护独立于采集开关，在页面顶部单独保存。切换为 `always` 可处理不携带模板的客户端请求，但仍需先有有效桶。旧宿主或缺少所选账号信息时会保守拒绝；插件 schema 6 是最低兼容标记，不能证明任意分支包含请求头转发修复。

受保护账号仅支持 HTTP/SSE。WebSocket 复用连接无法保证逐次注入新模板，因此这类请求会被拒绝；请将客户端切换到 HTTP/SSE。关闭保护会允许账号按普通规则发起请求，页面会要求确认。

| 设置 | 行为 |
| --- | --- |
| `replace-only`（默认） | 仅在请求头长度等于替换长度（默认 312）时注入有效模板 |
| `always` | 有匹配的有效模板时注入，包括请求未携带该头的情况 |
| 观察模式 `dry_run` | 记录决策，但不修改业务请求头 |
| 响应学习 | 从成功的 HTTP/SSE 响应头保存有效模板 |
| 默认探测模型 | `gpt-6-astra`、`gpt-5.6-sol`；与旧默认值完全一致的列表会自动纠正，自定义或清空的列表保留 |
| 模板长度 / 替换长度 | 默认 292 / 312，可按实际协议调整 |
| 有效期 | 从令牌内嵌时间戳计算，默认 3600 秒；重复采集不会延长令牌寿命 |

模板严格按 CPA 选中的账号和实际上游模型隔离。`always` 不会生成模板，也不会跨账号、模型使用；`pass` 可能表示请求已经带着当前模板，应结合界面中的具体原因判断。响应头长度是该第三方项目的识别规则，不代表对模型回答质量的独立验证。

选择探测账号、填写模型后，可开始探测并保持页面打开以连续采集。**已停用的 Codex OAuth 账号也可以选中并采集**，列表会标注停用状态；采集不会启用账号，日常业务仍遵守 CPA 的停用设置。账号需要有效的 OAuth 凭证；过期凭证需重新登录或由 CPA 刷新。已删除或不再是 Codex OAuth 的账号不参与探测，已保存的失效选择可在页面中移除。静态代理一行对应一个固定出口，同一账号/模型失败后冷却 55 分钟；轮换代理同一 URL 最多连续尝试 10 次，然后冷却 10 分钟。成功后按模板实际签发时间和有效期安排续采。提前续采可设置为 10、20 等整数分钟，必须小于模板有效期；`renew_before_minutes=0` 保留自动策略：提前 5 分钟，短有效期则提前有效期的四分之一。调整该设置会重新安排成功出口的续采，不重置失败出口的冷却。升级前保存的旧冷却没有成功标记，会先按原期限结束一次。

采集表格按「账号 × 模型」展示，单元格显示剩余有效分钟数，包含未采集的桶及范围外仍有效的模板。采集账号与代理默认展开，已保存代理会完整加载到文本框。支持带账号密码的 HTTP、HTTPS、SOCKS5、SOCKS5H URL；只提交有修改的池，把已加载的文本框内容删空并保存会清空对应池。状态接口仍只返回代理数量；完整代理通过管理员专用接口分页读取，并禁止缓存。

账号、模型、代理、注入和续采策略统一显示未保存状态，各区块和页面顶部均提供「保存所有设置」，失败时保留草稿。桶就绪度直接展示采集代理的 IP/主机及端口，认证信息仍隐藏；轮换代理网关地址不等于当次真实出口 IP。

总览分别展示探测结果、桶就绪度和业务决策计数，包括替换、补入、直通、跳过、观察模式及错误。计数从本次插件加载起计算，页面刷新不会清零。每个桶可定向采集、通过 CPA 发起自测、清除模板或清除冷却。自测固定该账号和模型，发送一条不携带模板的最小请求，用于检查 CPA 调用路径；不会采集模板，也不证明注入效果或回答质量。

「清除冷却」需要二次确认，只移除失败冷却，保留有效模板与正常续采等待。清除后下一轮可能马上重试，消耗上游额度、代理流量，并再次触发限流；它不会解除上游自身的限制。单桶操作同时清除该账号共享的账号级暂停，确认弹窗会说明影响。

每个池可单独测试连通性，显示进度、代理地址和可取得的出口 IP。测试不使用账号凭证、不消耗账号额度，连接 Codex 接口验证可达性；连接失败或代理认证失败才可批量删除，上游 403、429 或无法确定的结果会保留。「删除无效代理」先修改文本框，点击保存才生效。采集日志和实时进度显示当前代理序号/总数、脱敏地址，轮换池同时显示重试次数；成功模板也保留脱敏采集地址。代理地址与实际出口 IP 不一定相同，轮换代理的检测出口仅代表该次检测，不能证明后续采集使用相同 IP。

静态池和轮换池各支持最多 20,000 条代理，完整配置上限为 16 MiB。管理页保存大配置时自动分块上传，每次请求约 16 KiB，避免大量代理一次性提交触发常见网关的 `413 Request Entity Too Large`。全部上传并校验通过后才一次性生效；中途失败保留现有配置和页面草稿，可直接重试。加载代理后或上传期间若其他页面修改了配置，本次代理提交会被拒绝；请使用重新加载按钮取得最新列表后重新编辑、保存。未完成上传仅保留在内存，15 分钟无活动后在后续调用中清理，不写入状态文件。

探测使用 Go 内置 HTTP 客户端，无需服务器安装 `curl`。每次管理请求同步完成一次探测，总超时为 25 秒；每次新建连接，读取响应头后关闭响应和连接，避免轮换代理因连接复用而继续使用同一出口。探测使用选定账号直接请求 Codex，不修改 CPA 的账号启用状态或全局代理；**会消耗上游额度，直连探测不计入 CPA 下游 API Key 的计费记录**。连续任务仍由浏览器驱动：刷新页面后恢复任务，离开页面时暂停，返回后可恢复；多个同源、同管理身份的标签页协调运行，避免重复采集。点击停止会清除恢复意图，已发出的那次请求会结束或超时；关闭所有页面时服务器没有后台采集任务。保存配置和清空模板会等待当前探测结束，避免旧结果覆盖新设置。WebSocket 握手响应头不在当前 HTTP/SSE 学习接口中；可先通过探测取得模板。

设置、代理和模板保存在计费数据库旁的 `<state_file>.turn-state.json`，备份时一并保存。文件限制为当前系统用户可读写；管理 API 不返回原始模板；管理员专用代理读取接口会返回完整代理 URL（含密码），供文本框编辑。普通状态与采集日志中的地址仍脱敏。集成模块与独立 turn-state 插件应选择其一启用，避免两个模块先后改写同一请求头。

## 拦截请求的响应

| 场景 | 状态码 | `type` | `code` |
| --- | --- | --- | --- |
| API Key 并发已满 | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| 订阅额度用尽 | `429` | `rate_limit_error` | `rate_limit_exceeded` |
| 模型无权访问 | `403` | `permission_error` | `insufficient_quota` |
| 未分组默认拒绝、所属分组均禁用或为空组、最终凭证未授权 | `403` | `permission_error` | `access_denied` |
| 没有符合规则且可用的凭证 | `503` | `server_error` | `internal_server_error` |
| 已绑定的路由规则不存在或损坏 | `503` | `server_error` | `routing_configuration_error` |

通过 WebSocket 调用 `/v1/responses`（如 Codex CLI 的 WebSocket 传输）时，CPA 遇到插件拦截会直接关闭连接，客户端收不到上表的状态码和提示语，通常表现为连接断开后重试。

## 在 CPA 中调试本地修改

仓库根目录的 `registry.json` 使用 CPA 插件商店 schema v1，插件 ID 为 `cpa-team-manager`，仓库指向本项目。可把对应分支的 Raw URL 添加到 CPA 的插件商店源。商店安装依赖该仓库的 GitHub Release 构建产物；`registry.json` 本身不会编译或加载本地代码。

调试尚未发布的修改时，安装 Go 1.24+ 和 C 编译器，在本仓库根目录编译动态库。Windows PowerShell（需要 MinGW-w64 GCC）示例：

```powershell
$env:CGO_ENABLED = "1"
New-Item -ItemType Directory -Force dist | Out-Null
go build -buildvcs=false -tags cshared -buildmode=c-shared -o dist/cpa-team-manager.dll ./cmd/cpa-key-billing
```

Linux 使用：

```sh
mkdir -p dist
CGO_ENABLED=1 go build -buildvcs=false -tags cshared -buildmode=c-shared -o dist/cpa-team-manager.so ./cmd/cpa-key-billing
```

停止 CPA，将对应平台的动态库复制到其 `plugins/` 目录，按上文配置启用插件后重启。避免在根目录和平台子目录同时保留相同 ID 的旧版插件。打开 `/v0/resource/plugins/cpa-team-manager/ui` 即可调试；UI 嵌入动态库，每次修改后都需要重新编译并重启 CPA。测试时建议使用独立的 CPA 配置、数据库和虚拟 Key。

## 致谢

- [haowang02/cpa-plugin-key-billing](https://github.com/haowang02/cpa-plugin-key-billing) - 本仓库 fork 的原项目；已适配合入上游 v1.3.12 的中英文支持（[594c3b1](https://github.com/haowang02/cpa-plugin-key-billing/commit/594c3b1922edb6ce38058b4b1dc2707023ed0cf3)），保留本分支的分组授权与 Codex Turn State 扩展
- [cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state) - 集成的 Codex Turn State 模板采集与注入功能来源
- [LINUX DO](https://linux.do/) - 新的理想型社区
