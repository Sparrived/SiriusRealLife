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
internal/llm/        LLM 客户端。唯一实现是 AMKR 的 OpenAI 兼容接口（见第 5 节）
internal/memory/     意识流三层：stream / working / longterm
internal/transport/  HTTP 路由 + SSE 推送 + AMKR WebUI 反代
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

**R9. LLM 只有一个出口：AMKR。**
Sirius 不直接调用任何模型供应商，不引入供应商 SDK，代码里不出现真实模型名。细节见第 5 节。

**R10. Sirius 不重试 LLM 调用。**
重试、切 Key、冷却全部由 AMKR 负责。客户端再重试会造成双倍计费和日志噪音。细节见第 5 节。

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

## 5. LLM 接入：AMKR 是唯一出口

所有 LLM 调用都走本地 **AMKR**（auto-model-key-router）的 OpenAI 兼容接口。
AMKR 是**独立仓库、由独立会话维护**，Sirius 只消费它的 HTTP API。

### 5.1 客户端

- 唯一实现：`internal/llm/openai.go`，用 `net/http` + `encoding/json` 手写 OpenAI Chat Completions 客户端
- **禁止**引入任何供应商或框架 SDK（openai-go、langchain 之类）。AMKR 已把多供应商、多 Key、协议差异吸收掉了，Sirius 再加一层适配纯属负债
- 连接信息只从环境变量读，写进 `.env.example`：
  - `AMKR_BASE_URL`，默认 `http://127.0.0.1:8000`
  - `AMKR_API_KEY`
- **key 不准**出现在代码、状态表、prompt 或日志里。日志里要标识 key 就用指纹，不打明文

### 5.2 模型名：调用点用任务名，不写真实模型名

- Go 代码里**不出现**真实模型名（`gpt-*`、`claude-*` 等）。调用点把 `TASK_XXXXXX` 当 `model` 传，真实模型与采样参数在 AMKR 的 WebUI 里配
- 这样换模型、调温度不需要改 Sirius 的代码，也不需要重启 —— 正好满足"人格参数可调"的诉求
- 调用点 → 任务名的映射放在一张显式的表里（v1 硬编码在 Go 里，允许环境变量覆盖单个条目）。建议至少分开：状态分派、内心独白、工具结果解读
- **用任务路由时不要传** `temperature`/`top_p`/`top_k`/`frequency_penalty`/`presence_penalty`/`seed`/`stop`：AMKR 对任务里已固定的参数会直接返回 `400`（它宁可报错也不静默覆盖，这样调用方不会误以为自己传的值生效了）。`reasoning_effort` 是例外，可以传
- 不在任务 `params` 里的参数（如 `max_tokens`）正常透传

### 5.3 超时、重试、流式

- **Sirius 不重试 LLM 调用。** AMKR 已负责重试、切换 Key、冷却异常 Key。客户端重试 = 双倍计费 + 日志噪音
- `503`（没有可用 Key）当作可降级错误处理：进入降级状态并把情况记进意识流，**不要**立刻重试
- 超时全部由 `context` 控制，且 **Sirius 的超时预算必须大于 AMKR 的**（AMKR 默认 `request_timeout=60s`、`stream_first_byte_timeout=90s`、`stream_idle_timeout=180s`）。客户端先超时会把 AMKR 正在重试的请求提前掐死，白花钱
- 流式调用一律带 `stream_options.include_usage=true`（AMKR 会强制补上），解析时按"usage 可能存在"处理，不假设它一定在最后一个 chunk
- 所有调用必须接受可取消的 `context`：状态被抢占时取消在途调用，别让它跑完 30 秒再丢弃（对应 R4）
- AMKR 可能把请求切到任意一个配了同一个模型的 Key，**响应头、字节序、错误格式都按 OpenAI 标准处理，不要依赖某个上游的私有行为**

### 5.4 内嵌 AMKR WebUI

管理页面不自己写，直接反代 AMKR 自带的那套（`/ui/`）。这是**部署期约束**，实现时要守住：

- Go 在 `/amkr/` 挂一个 `httputil.ReverseProxy` 转发到 AMKR，**路径 1:1 透传，不改写任何段**。AMKR 前端从当前 URL 里的 `/ui/` 段反推 API 基址，重写掉这段会让所有管理请求打到错误路径
- `Authorization: Bearer $AMKR_API_KEY` 由 Go **在服务端注入**，密钥下发给浏览器就等于泄露
- 必须 **403 掉 `/amkr/api/service/*` 和 `/amkr/api/integrations/*`**。这两个是"操作宿主机"的运维接口（启停进程、注册系统服务、改写本机 Claude Code / Codex 配置），在容器里语义不成立，而且会写脏配置
- `/amkr/` 等同于 AMKR 的完整管理权限。**在 Sirius 自己具备鉴权之前，服务只能绑 `127.0.0.1`**，不得暴露到局域网或公网
- 不代理 AMKR 的 `/ws/events`：WebUI 不用它（只用 fetch + 轮询），没必要处理升级
- AMKR 的配置文件、metrics sqlite、上游 key 都在 AMKR 那边管理，**不进本仓库**，Sirius 也不读它们

### 5.5 进程模型与部署

- v1 用 docker compose 两个容器：`amkr` + `sirius`，Sirius 通过服务名访问 `http://amkr:8000`
- 单镜像双进程是**后续可选的分发优化，不是 v1 目标**。先把链路跑通，再谈合并
- 就绪与存活判断打 AMKR 的 `/health`（该接口免鉴权），不要用 `/` 或猜端口
- AMKR 镜像的 Dockerfile、tzdata 依赖等由 AMKR 仓库那边负责，**不在本仓库处理**。Sirius 只需假设 AMKR 的 HTTP 契约可用

---

## 6. 提交与协作

- Conventional Commits：`feat:` `fix:` `refactor:` `chore:` `docs:`
- 中文或英文正文都可以，一句话说清"为什么"，diff 已经说明"做了什么"
- 不留注释掉的死代码，不留 `TODO` 而不写跟进条件；要留就写 `ponytail: <上限>，<升级路径>`
- 非平凡逻辑（分支、循环、解析、钱/权限路径）必须留下**一个**能跑的检查：`demo()` 形式的自检或一个小 `_test.go`。不搭测试框架，不写每函数一套 fixture
- 机密（API key、token）永不入库。`.env` 已在 `.gitignore`，只提交 `.env.example`
- **本仓库已公开。** 提交前自查：没有上游 key、没有 AMKR 的 `local_api_key`、没有 `router-config.json`

---

## 7. 反目标（明确不做）

这些是刻意砍掉的，不是忘了。想加先在此文档里改约定，再写代码。

- ❌ DDD 分层、聚合根、领域事件总线
- ❌ 事件溯源 / CQRS
- ❌ 插件系统、工具运行时注册表、反射扫描
- ❌ 抽象工厂、只有一个实现的接口
- ❌ 微服务、消息队列、K8s —— 单体一个进程跑起来再说
- ❌ 为"以后可能要多用户"提前做多租户
- ❌ 自建 LLM 供应商适配层 / SDK 封装（AMKR 已经做了）
- ❌ 自建管理后台（直接反代 AMKR 的 WebUI）
- ❌ 在 Sirius 里做重试、Key 轮询、配额统计（AMKR 的职责）

---

## 8. MVP 范围（第一版只做这些）

- 1 个 agent，4 个状态（刷手机 / 发呆 / 工作 / 找人聊天）
- 1 个工具（`read_app`）
- 固定 tick 循环，状态表硬编码在 Go 里
- LLM 走 AMKR，调用点先用**一个**任务名（如 `TASK_000001`），verify 通链路后再拆分
- SSE 推流转到 Vue，页面显示实时意识流 + 状态图（当前节点高亮）
- `/amkr/` 反代可用，能在 Sirius 页面上切到 AMKR WebUI 配模型
- 验收：连续跑 30 个 tick 不自锁、不死循环、每次转移都有日志；LLM 调用期间状态机不被阻塞

跑通之后再考虑：多个 agent 互动、心境影响权重、长期记忆压缩、配置热加载、单镜像分发。

---

## 9. 待定

| 项 | 状态 | 说明 |
|---|---|---|
| LLM 接入 | **已定** | 统一走 AMKR 的 OpenAI 兼容接口，调用点用 `TASK_XXXXXX` 任务名。见第 5 节 |
| 进程模型 | **已定（v1）** | docker compose 两容器；单镜像双进程留作后续优化 |
| 任务名划分 | 待定 | 先用一个任务跑通，之后按调用点（状态分派 / 内心独白 / 工具解读）拆 |
| Sirius 自身鉴权 | **阻塞项** | 没有它就不能把 `/amkr/` 暴露到 localhost 之外 |
| 持久化 | 暂不需要 | v1 全内存，重启即清零 |
| 前端设计风格 | 待定 | 参考 `design-taste-frontend` 技能 |
