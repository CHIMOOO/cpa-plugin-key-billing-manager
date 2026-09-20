# State 相关项目审查（2026-09-20）

首轮审查核对公开来源、近期版本和移植可行性。可选严格主动探测、滚动小时预算、模型测试响应声明模型，以及 CPA 图片端点的 State 隔离已纳入 v0.0.11。当天再次核对后，新增响应 State 观测与单张模板持久丢弃，纳入 v0.0.13；实际操作与最新对齐表以 README 为准。

## 第二次核对：arden main 0.3.0 开发版、NanSsye r18 预发布版

- **arden**：正式 Release 和 tag 仍是 `v0.2.0` / `1a62523e8fdc67c4d83555136c74aabd6f9f9d9d`。本次审核 main [`d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9`](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/commit/d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9)，[比正式版新增 16 个提交](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/compare/1a62523e8fdc67c4d83555136c74aabd6f9f9d9d...d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9)。源码版本是 0.3.0，不称为已经发布的 v0.3.0。
- **NanSsye**：仓库已改名为 [`ccodex-sleep-state`](https://github.com/NanSsye/ccodex-sleep-state)，旧地址会跳转。本次核对 [`r18`](https://github.com/NanSsye/ccodex-sleep-state/releases/tag/r18) / `8cab81c9426e36be6cabbd8f38803afb33f75aac`，相对首轮基线新增 r17、r18 两个提交。r18 发布于 2026-09-20 12:02:22 UTC，属于 **prerelease**；该仓库 `/releases/latest` 返回 404 不代表没有发布记录。

| 新增变化 | 本工作区处理 |
| --- | --- |
| arden 响应 State 观测 | 已适配：精确账号／模型归属、本插件实际注入标记、七类长度观测、最近 100 事件、最多 256 桶、按小时汇总近 24 小时。业务响应回调只更新有界内存，不落盘、不创建后台任务；重启清空，明确区别于上游持久化实现。 |
| arden 观测判断修复 | 未知长度独立显示、dry-run 不记实际注入、模板存在不等于近期在注入；最近非空头独立保存长度，后续无头不会把它改成 0。不以长度、静默或是否有记录推断模型质量、业务成功率或流量。 |
| arden 重试归属修复 | 本插件宿主封装已有 early-return 前清理；补充 Manager 层重试归属隔离，以及缺账号、换账号／模型和注入标记的回归验证。 |
| NanSsye r17 单票持久丢弃 | 参考行为独立实现：管理员按账号／模型与完整指纹精确丢弃，陈旧点击拒绝；原子保存后生效，同票在签发满 1 小时前不能通过学习、采集或同目录重启回填。保留原有清空语义，没有引入多票 FIFO 池。 |
| NanSsye r18 节点连接恢复 | 上游连续两次 I/O 失败后更换共享节点连接池代次，并等待旧请求结束；与它的长期复用 Mihomo/HTTP transport 有关。我们每个采集请求使用独立 transport 并立即关闭，业务连接由 CPA 管理，不加入无对应生命周期的重建逻辑。 |

新增源码依据：

- [arden 观测行为与边界](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/blob/d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9/README.md#L58-L87)、[响应长度分类](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/blob/d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9/go/observations.go#L349-L435)、[实际注入及响应头 hooks](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/blob/d3efa48cec2ad4a973c5c7c1c3e794c7b7d7cae9/go/main.go#L1183-L1248)。
- [arden 缺账号重试归属修复](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/commit/46c467891c7a06d649c7b46169de07c01eecc528)。
- [NanSsye r17 精确模板丢弃提交](https://github.com/NanSsye/ccodex-sleep-state/commit/41f83eef19314b2ed9120d34168d3aac02076cb6)、[r18 连接恢复提交](https://github.com/NanSsye/ccodex-sleep-state/commit/8cab81c9426e36be6cabbd8f38803afb33f75aac)。

下文保留首轮 r16 / v0.2.0 的来源与功能审查范围，不能将其中的“当前”理解为第二次核对的最新版本。

## 来源关系与版本范围

- 我们已集成的直接来源是 [arden-aaai/cpa-plugin-codex-turn-state](https://github.com/arden-aaai/cpa-plugin-codex-turn-state)，原参考提交为 `13573a6`；本次核对到 [v0.2.0 / 1a62523](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/releases/tag/v0.2.0)。最新版本主要重做控制台，发布说明明确默认参数、探测规则与存储格式不变。
- [NanSsye/ccodex-sleep-state-r14](https://github.com/NanSsye/ccodex-sleep-state-r14) 是 [gylive/ccodex-sleep-state](https://github.com/gylive/ccodex-sleep-state) 的 GitHub fork。核对提交为 `b40529797cc675b09a2f93024a5ecc3e1d90c8cb`，当前独立 Release 为 [state-pool-20260920-r16](https://github.com/NanSsye/ccodex-sleep-state-r14/releases/tag/state-pool-20260920-r16)。r14、r15 是提交中的功能阶段，不是各自都有一个正式 Release。
- arden 的当前源码、README、FINDINGS 和公开提交历史中，未找到 NanSsye、gylive 或 ccodex-sleep-state 的引用。arden [首个插件实现](https://github.com/arden-aaai/cpa-plugin-codex-turn-state/commit/b4eaa1f8c239f7c86ad415d0a10d0df5fdbd2384)发表于 2026-09-17，NanSsye 仓库创建于 2026-09-20。公开证据不足以把 NanSsye 称为 arden 的“鼻祖”；也不能据此断言双方从未受到共同思路启发。
- arden 为 MIT；NanSsye 为 GPL-3.0，且内嵌 Mihomo。本仓库为 MIT。以下建议是参考功能行为后独立设计；直接复制其 Go、UI 或内嵌核心，需要另行评估许可与发行要求，增加链接不能替代该评估。

## 近期变化与本项目适用性

| 功能 | 上游行为 | 我们当前情况与建议 |
| --- | --- | --- |
| 自动采集小时预算 | 全局滚动一小时预算，发请求前持久化预留；重启不重置 | **已独立实现，可选开启。** `probe_hourly_limit` 默认 0 不限，所有桶、池及手动探测共享。耗尽后显示恢复倒计时，runner 继续运行，旧模板继续使用；不照搬默认 30 次，多账号续采可能不够。 |
| 探测完成校验 | 有界读取 SSE，确认 completed 且无 failed/incomplete/error 后接受 State | **已独立实现，默认关闭。** `probe_verify_completion` 开启后，主动探测在 25 秒、解码后 1 MiB 上限内确认 SSE/JSON 正常完成，再验证 State 长度与签发时间。只作用于主动探测，不缓存业务正文或构造计费；可能增加耗时和输出消耗。 |
| 主用与备用模板 | 同一会话保留主用及若干备用，FIFO 提升并私有备份 | **可以独立设计，后续再做。** 我们已有提前续采、旧票继续使用、原子替换、持久化及重启恢复，但每桶只有一张。备用必须按账号/模型隔离、去重并保留真实到期时间；不能跨账号共享或自动重放已经发送的业务请求。 |
| r10 图片接口修复 | 修复独立网关本地 unsupported_endpoint 404，放行生图/改图等功能接口；保留 multipart/二进制/压缩原始内容，图片首响应头等待最长 10 分钟；非聊天不采集、不注入、不按 State 删除主备 | **已修复插件 State 隔离，未照搬网关。** CPA 两个明确图片端点跳过聊天 State 门槛、注入及学习，保留分组、额度与并发。真实 CPA v7.3.8 + 模拟上游已验证生图/改图通路；宿主转发和超时仍由 CPA 管理，未声称改变模型能力。 |
| r15 上游响应模型显示 | 读取本次 SSE 的 response.model 或 JSON model，并标记与请求不一致 | **已加到显式模型测试结果。** 分开显示填写模型、发送模型与上游声明模型，标明缺失、不一致或冲突。业务 UsageRecord 没有独立的响应声明模型字段，不从请求模型冒充、不猜测关联；该字段不证明真实身份或质量。 |
| r16 压缩请求携带主票 | 已有有效票且启用注入时携带，绑定来源节点；没有票不等采集 | **先做 CPA 专项兼容性验证。** 我们通用注入没有主动排除 compact，但宿主是否让该路由经过精确账号/模型钩子、最终有没有转发请求头，需要实际验证。不能直接宣称已经完整支持或一定缺失，也不应把压缩改成普通生成。 |
| r16 手动打票绕过等待 | 随机单次，跳过预算预留、节点等待及部分已有上游暂停 | **不照搬。** 可以提供立即单次动作，但仍应保留明确的上游 429/认证拒绝和用户预算；至多跳过普通本地轮间等待。见下面源码差异。 |
| 模板与采集来源节点绑定 | 独立网关同时控制请求 State 和 HTTP 拨号器，固定到来源节点 | **当前插件不直接移植。** 我们记录采集来源而不改变 CPA 的业务出口；请求拦截接口的返回值没有每请求代理覆盖字段。修改账号 proxy_url 会影响并发业务，不能冒充原子绑定。同一轮换网关也不能保证同一公网 IP。 |
| r11 随机节点轮次 | 按账号/模型持久化已尝试节点，跨批次和重启继续 | **目的已部分覆盖，保留我们的策略。** v0.0.11 采用用户要求的全局顺序游标，成功后跨桶顺延，重启与删代理后继续；不需要改回上游每会话随机顺序。 |
| 空闲停止与网络失败保票 | 空闲 300 秒停止补采；普通网络/5xx 失败不删除有效票 | **保票已有；不默认采用空闲停止。** 我们失败续采不会清空仍有效的旧模板。用户要求服务器持续准备模板，桌面单用户的空闲停止策略不适合作为默认值。 |

## 核对中的关键边界

1. **r16 的手动采集不只是“跳过本地倒计时”。** `manualRandom` 跳过预算和 `probeSchedule`，已有 429 等待也可绕过；只保留 credentialLimit 中的 401/403 硬拒绝。探测产生的拒绝主要写入 session.upstreamPause，不能把“保留认证检查”理解成保留所有探测暂停。因此不建议直接移植其手动规则。
2. **不能把所有错误改为模型级。** 我们模板、出口冷却和轮换预算已经按账号/模型隔离；明确 HTTP 401/403/429 仍暂停账号。只有证据足够说明错误来自单模型容量时才适合细分，不能仅凭某种短 State 长度就解除账号保护。
3. **备用不等于一定无空档。** 多张模板可能接近同时到期；预算、代理故障、上游拒绝仍会使续采失败。不得延长签发时间或把过期模板继续标为有效。
4. **采集成功不等于能力鉴定。** 合格长度、生成完成或响应模型名一致，都不能独立证明未降智。上游也没有给出迁移这些功能就能提高成功率的可复现实验。

## 源码依据

以下 NanSsye 链接均固定到本次审查提交，避免后续 main 变更改变结论。

- [累计更新记录：r16、r15、r13 至 r9](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/CHANGELOG.md#L3-L51)
- [r10 图片修复说明](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/CHANGELOG.md#L37-L42)、[功能接口分类与转发范围](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/endpoints.go#L10-L48)
- [滚动预算与发出前预留](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/collection_budget.go#L133-L183)
- [主动探测：有界读取及完成检查](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/engine.go#L669-L768)
- [手动随机模式、暂停与预算分支](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/engine.go#L450-L597)
- [主备 FIFO](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/turnstate/store.go#L121-L182)、[备份隔离](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/turnstate/backup.go#L21-L22)
- [压缩请求与出口绑定](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/handler.go#L147-L281)、[响应声明模型读取](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/internal/gateway/response_model.go#L22-L54)
- [GPL 与内嵌依赖说明](https://github.com/NanSsye/ccodex-sleep-state-r14/blob/b40529797cc675b09a2f93024a5ecc3e1d90c8cb/THIRD_PARTY_NOTICES.md#L5-L14)

本项目对照：[`probe.go`](../internal/turnstate/probe.go)、[`probe_http.go`](../internal/turnstate/probe_http.go)、[`manager.go`](../internal/turnstate/manager.go)、[`turn_state.go`](../internal/plugin/turn_state.go)、[`types.go`](../internal/plugin/types.go)、[`statecollector`](../internal/statecollector/collector.go)。

实施进展：可选小时预算、有界主动探测校验、模型测试中的响应声明模型，以及本次响应 State 观测和单模板持久丢弃已独立实现；图片修复只处理 CPA 明确图片端点的 State 误拦与学习隔离。备用模板、compact 专项验证、来源节点绑定及上游手动绕过暂停规则未加入。我们提供的用户可配置冷却与不限次数模式，仍保留串行间隔、独立小时预算和有效模板续采规则。
