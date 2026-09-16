# AGENTS.md

本项目（SiriusRealLife）的工程约定。所有人和 agent 在此仓库工作时都必须遵守本文档。
约定与代码冲突时，改代码。

---

## 1. 项目是什么

一个人格模拟器。人格是一个**有限状态机 + 意识流**：

- 任一时刻只持有**一个**状态
- 状态由**随机分派器**（带权重、冷却、前置条件）选出，也可被事件抢占
- 状态 = 一段有明确出口的过程：目标 + 可用工具白名单 + 退出条件
- 时间由固定 **tick** 驱动，随机只在"该换状态了"这一刻介入

技术栈：**Go** 后端 + **Vue 3** 前端。

---

## 2. 目录结构

```
cmd/agent/           程序入口，只做装配（读配置、建 agent、起 http），不放业务逻辑
internal/fsm/        状态机核心：状态定义、分派器、tick 循环、状态转移
internal/mood/       心境（连续量），影响分派权重与工具参数
internal/tools/      工具实现，每个工具一个文件
internal/llm/        LLM 供应商适配，统一接口
internal/memory/     意识流三层：stream / working / longterm
internal/transport/  HTTP 路由 + SSE 推送
config/              状态表、权重、prompt 模板
web/                 Vue 3 + Vite + TS 前端
```

新目录必须有明确归属，不进 `internal/` 就别建。禁止 `utils/`、`common/`、`helpers/`、`models/` 这类无主题垃圾桶。

---

## 3. 硬规则（违反会出 bug 的那几条）

**R1. 一个 agent 一个 goroutine，状态只被它自己改。**
事件从 channel 进，tick 循环用 `select` 收。其他 goroutine 想影响 agent，只能投递事件，**不准**持锁直接改状态。违反这条会出一类极难复现的竞态。

**R2. 状态与心境正交，不合并。**
- 状态：离散枚举，有出口条件
- 心境：连续量（精力/烦躁/好奇/亲密度），自己按时间衰减

影响方向是单向的：心境 → 分派权重、心境 → prompt 与工具参数、状态 → 工具白名单。
**不准**把情绪写进状态名（`ANGRY_WORKING` 是错的），也**不准**让状态直接改心境以外的全局。

**R3. 纯随机会毁掉人格。**
分派必须是加权抽取，每个状态至少声明：`weight`、`guard`（前置条件）、`minTick`/`maxTick`（持续时长）、`cooldown`。并且：
- 同一状态不允许连续进入两次，除非被事件强制
- 允许高优先级事件抢占当前状态
- 抽取结果必须可复现：agent 持有自己的 `*rand.Rand`，种子显式传入，不用全局 `math/rand`

**R4. LLM 调用是阻塞 IO，绝不能挡住 tick。**
进行中的调用是一个**显式状态**（如 `thinking`）。调用期间 agent 照常收事件、照常可被抢占。禁止在 tick 循环里同步 `await` 一次 30 秒的调用。

**R5. 意识流必须有界。**
三层分开：`stream`（全量日志，可归档）、`working`（最近 N 条，进 prompt）、`longterm`（压缩摘要）。
prompt 只读 `working` + `longterm`。任何往 prompt 里塞全量历史的代码都是 bug。

**R6. 每次状态转移都留下结构化日志。**
`from`、`to`、`reason`（timeout/event/dispatch/preempt）、当时候选状态的权重快照、随机数。
这是唯一的调试器，也是将来回归测试的输入。用 `log/slog`，字段不用字符串拼。

**R7. 工具签名统一。**
```go
type Tool func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
```
工具只是给 LLM 的能力白名单，**不准**在工具里写业务逻辑或状态机逻辑。状态声明所需工具写 `Tools: []string{"read_app"}` 即可，禁止工具类继承、插件注册表、反射发现。

**R8. 时间是一等公民。**
业务逻辑只认 tick 序号，不认 `time.Now()`。真实时间与游戏时间的换算只在 tick 源头做一次。这样离线推演、加速、回放全都免费。

---

## 4. 编码约定

**Go**
- Go 1.26+，模块 `github.com/Sparrived/SiriusRealLife`
- **标准库优先**。HTTP 用 `net/http` + Go 1.22 起的 `ServeMux` 路由模式，不引 gin/echo。JSON 用 `encoding/json`。日志用 `log/slog`。配置用 `encoding/json` 或直接写 Go 表
- 加依赖前先问：标准库能不能做？已有依赖能不能做？两个都不能才加，并在 PR/commit 里说明原因
- 状态表 v1 直接写在 Go 源码里（编译期可查错）。等真的需要不重启调人格时，再迁到 `config/` 并做热加载
- 错误必须 `fmt.Errorf("...: %w", err)` 包装，禁止裸 `return err` 丢上下文
- `context.Context` 作为第一个参数贯穿所有 IO
- 不用 ORM。要持久化就 `database/sql` + 具体驱动

**Vue**
- Vue 3 `<script setup>` + TypeScript + Vite
- 状态管理优先 `ref`/`computed`，跨组件才上 Pinia
- 后端实时流用 **SSE**（`EventSource`），不上 WebSocket、不上 Socket.IO
- 组件文件 PascalCase，组合式函数 `useXxx.ts`

**API**
- 统一前缀 `/api/v1`
- 请求/响应 JSON，字段 `snake_case`
- 实时流：`GET /api/v1/agents/{id}/stream`（SSE，事件类型标识消息种类）
- 错误响应：`{"error": "...", "detail": "..."}`，HTTP 状态码如实反映

---

## 5. 提交与协作

- Conventional Commits：`feat:` `fix:` `refactor:` `chore:` `docs:`
- 中文或英文正文都可以，一句话说清"为什么"，diff 已经说明"做了什么"
- 不留注释掉的死代码，不留 `TODO` 而不写跟进条件；要留就写 `ponytail: <上限>，<升级路径>`
- 非平凡逻辑（分支、循环、解析、钱/权限路径）必须留下**一个**能跑的检查：`demo()` 形式的自检或一个小 `_test.go`。不搭测试框架，不写每函数一套 fixture
- 机密（API key、token）永不入库。`.env` 已在 `.gitignore`，只提交 `.env.example`

---

## 6. 反目标（明确不做）

这些是刻意砍掉的，不是忘了。想加先在此文档里改约定，再写代码。

- ❌ DDD 分层、聚合根、领域事件总线
- ❌ 事件溯源 / CQRS
- ❌ 插件系统、工具运行时注册表、反射扫描
- ❌ 抽象工厂、只有一个实现的接口
- ❌ 微服务、消息队列、K8s —— 单体一个进程跑起来再说
- ❌ 为"以后可能要多用户"提前做多租户

---

## 7. MVP 范围（第一版只做这些）

- 1 个 agent，4 个状态（刷手机 / 发呆 / 工作 / 找人聊天）
- 1 个工具（`read_app`）
- 固定 tick 循环，状态表硬编码在 Go 里
- SSE 推流转到 Vue，页面显示实时意识流 + 状态图（当前节点高亮）
- 验收：连续跑 30 个 tick 不自锁、不死循环、每次转移都有日志

跑通之后再考虑：多个 agent 互动、心境影响权重、长期记忆压缩、配置热加载。

---

## 8. 待定

| 项 | 状态 | 说明 |
|---|---|---|
| LLM 供应商 | **待定** | OpenAI / DeepSeek / 本地模型，定后写入 `internal/llm` 并在此登记 |
| 持久化 | 暂不需要 | v1 全内存，重启即清零 |
| 前端设计风格 | 待定 | 参考 `design-taste-frontend` 技能 |
