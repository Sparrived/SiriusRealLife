# AGENTS.md

本项目（SiriusRealLife）的工程约定。所有人和 agent 在此仓库工作时都必须遵守本文档。
约定与代码冲突时，改代码。

**本文只放两样东西**：必须时刻生效的硬规则，和文档索引。背景、理由、实施细则都在 [`docs/`](docs/)。
改约定就改文档，别只改代码。

---

## 1. 这是什么

一个人格模拟器：人格 = **有限状态机 + 意识流**。

- 任一时刻只持有**一个**状态
- 状态由**随机分派器**（带权重、冷却、前置条件）选出，也可被事件抢占
- 状态 = 一段有明确出口的过程：目标 + 可用工具白名单 + 退出条件
- 时间由固定 **tick** 驱动，随机只在"该换状态了"这一刻介入

技术栈：**Go** 后端 + **Vue 3** 前端。LLM 全部经 AMKR。

---

## 2. 硬规则

违反这些会直接出 bug。理由与实施细则见对应文档。

**R1. 一个 agent 一个 goroutine，状态只被它自己改。**
事件从 channel 进，tick 循环用 `select` 收。其他 goroutine 想影响 agent，只能投递事件，**不准**持锁直接改状态。违反这条会出一类极难复现的竞态。

**R2. 状态与心境正交，不合并。**
状态是离散枚举、有出口条件；心境是连续量（精力/烦躁/好奇/亲密度），自己按时间衰减。
影响方向**单向**：心境 → 分派权重、心境 → prompt 与工具参数；状态 → 只能改心境和意识流。
**不准**把情绪写进状态名（`ANGRY_WORKING` 是错的）。→ [architecture.md](docs/architecture.md)

**R3. 纯随机会毁掉人格。**
分派必须是加权抽取。每个状态至少声明 `weight`、`guard`、`minTick`/`maxTick`、`cooldown`。并且：
- 同一状态不允许连续进入两次，除非被事件强制
- 允许高优先级事件抢占当前状态
- 抽取必须可复现：agent 持有自己的 `*rand.Rand`，种子显式传入，不用全局 `math/rand`

**R4. LLM 调用是阻塞 IO，绝不能挡住 tick。**
进行中的调用是一个**显式状态**（如 `thinking`）。调用期间 agent 照常收事件、照常可被抢占，且必须能被 `context` 取消。禁止在 tick 循环里同步等一次 30 秒的调用。

**R5. 意识流必须有界。**
三层分开：`stream`（全量日志，可归档）、`working`（最近 N 条，进 prompt）、`longterm`（压缩摘要）。
**prompt 只读 `working` + `longterm`。** 任何往 prompt 里塞全量历史的代码都是 bug。

**R6. 每次状态转移都留下结构化日志。**
`from`、`to`、`reason`（timeout/event/dispatch/preempt）、当时候选状态的权重快照、随机数。
这是唯一的调试器，也是回归测试的输入。用 `log/slog`，字段不用字符串拼。

**R7. 工具签名统一。**
```go
type Tool func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
```
工具只是给 LLM 的能力白名单，**不准**在工具里写业务逻辑或状态机逻辑。状态声明所需工具写 `Tools: []string{"read_app"}` 即可，禁止工具类继承、插件注册表、反射发现。

**R8. 时间是一等公民。**
业务逻辑只认 tick 序号，不认 `time.Now()`。真实时间与游戏时间的换算**只在 tick 源头做一次**。这样离线推演、加速、回放全都免费。

**R9. LLM 只有一个出口：AMKR。**
Sirius 不直接调用任何模型供应商，不引入供应商 SDK，代码里不出现真实模型名（用 `TASK_XXXXXX` 任务名）。→ [llm-amkr.md](docs/llm-amkr.md)

**R10. Sirius 不重试 LLM 调用。**
重试、切 Key、冷却全部由 AMKR 负责。客户端重试 = 双倍计费 + 日志噪音。

---

## 3. 文档索引

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 状态/心境/分派器/tick/意识流五要素、数据流、目录结构 |
| [docs/conventions.md](docs/conventions.md) | Go / Vue / API 编码约定、提交规范、文档规范 |
| [docs/llm-amkr.md](docs/llm-amkr.md) | AMKR 接入全部细则：客户端、任务路由、超时、WebUI 反代、部署 |
| [docs/roadmap.md](docs/roadmap.md) | 反目标、MVP 范围与验收标准、待定项 |

---

## 4. 不可协商的两条安全线

1. **本仓库已公开。** 提交前自查：没有上游 key、没有 AMKR 的 `local_api_key`、没有 `router-config.json`。
2. **Sirius 自身具备鉴权之前，服务只能绑 `127.0.0.1`。** `/amkr/` 反代等同于 AMKR 的完整管理权限。
