# LLM 接入：AMKR 是唯一出口

所有 LLM 调用都走本地 **AMKR**（auto-model-key-router）的 OpenAI 兼容接口。
AMKR 是**独立仓库、由独立会话维护**，Sirius 只消费它的 HTTP API。

硬规则 **R9**（只有一个出口）与 **R10**（不重试）是本节所有条款的总纲。

---

## 1. 客户端

- 唯一实现：`internal/llm/openai.go`，用 `net/http` + `encoding/json` 手写 OpenAI Chat Completions 客户端
- **禁止**引入任何供应商或框架 SDK（openai-go、langchain 之类）。AMKR 已把多供应商、多 Key、协议差异吸收掉了，Sirius 再加一层适配纯属负债
- 连接信息只从环境变量读，写进 `.env.example`：
  - `AMKR_BASE_URL`，默认 `http://127.0.0.1:8000`
  - `AMKR_API_KEY`
- **key 不准**出现在代码、状态表、prompt 或日志里。日志里要标识 key 就用指纹，不打明文

## 2. 模型名：调用点用任务名，不写真实模型名

- Go 代码里**不出现**真实模型名（`gpt-*`、`claude-*` 等）。调用点把 `TASK_XXXXXX` 当 `model` 传，真实模型与采样参数在 AMKR 的 WebUI 里配
- 这样换模型、调温度不需要改 Sirius 的代码，也不需要重启 —— 正好满足"人格参数可调"的诉求
- 调用点 → 任务名的映射放在一张显式的表里（v1 硬编码在 Go 里，允许环境变量覆盖单个条目）。建议至少分开：状态分派、内心独白、工具结果解读

### 用任务路由时的参数规则

**不要传** `temperature`/`top_p`/`top_k`/`frequency_penalty`/`presence_penalty`/`seed`/`stop`：
AMKR 对任务里已固定的参数会直接返回 `400`（它宁可报错也不静默覆盖，这样调用方不会误以为自己传的值生效了）。

`reasoning_effort` 是例外，可以传。

不在任务 `params` 里的参数（如 `max_tokens`）正常透传。

> 任务路由把「模型 + 固定采样参数」打包成一个可直接当 `model` 传的名字。任务名不能与模型 ID、别名、隐藏别名或 `unified-model` 撞名，也不能指定 Key。

## 3. 超时、重试、流式

- **Sirius 不重试 LLM 调用。** AMKR 已负责重试、切换 Key、冷却异常 Key。客户端重试 = 双倍计费 + 日志噪音
- `503`（没有可用 Key）当作可降级错误处理：进入降级状态并把情况记进意识流，**不要**立刻重试
- 超时全部由 `context` 控制，且 **Sirius 的超时预算必须大于 AMKR 的默认值**：

  | 参数 | AMKR 默认 |
  |---|---|
  | `request_timeout` | 60s |
  | `stream_first_byte_timeout` | 90s |
  | `stream_idle_timeout` | 180s |

  客户端先超时会把 AMKR 正在重试的请求提前掐死，白花钱。

- 流式调用一律带 `stream_options.include_usage=true`（AMKR 会强制补上），解析时按"usage 可能存在"处理，**不假设它一定在最后一个 chunk**
- 所有调用必须接受可取消的 `context`：状态被抢占时取消在途调用，别让它跑完 30 秒再丢弃（对应 R4）
- AMKR 可能把请求切到任意一个配了同一个模型的 Key，**响应头、字节序、错误格式都按 OpenAI 标准处理，不要依赖某个上游的私有行为**

## 4. 内嵌 AMKR WebUI

管理页面不自己写，直接反代 AMKR 自带的那套（`/ui/`）。这是**部署期约束**，实现时要守住：

- Go 在 `/amkr/` 挂一个 `httputil.ReverseProxy` 转发到 AMKR，**路径 1:1 透传，不改写任何段**。AMKR 前端从当前 URL 里的 `/ui/` 段反推 API 基址（`apiBase()` 取 `lastIndexOf("/ui/")` 之前的部分），重写掉这段会让所有管理请求打到错误路径
- `Authorization: Bearer $AMKR_API_KEY` 由 Go **在服务端注入**，密钥下发给浏览器就等于泄露
- 必须 **403 掉 `/amkr/api/service/*` 和 `/amkr/api/integrations/*`**。这两个是"操作宿主机"的运维接口（启停进程、注册系统服务、改写本机 Claude Code / Codex 配置），在容器里语义不成立，而且会写脏配置
- `/amkr/` 等同于 AMKR 的完整管理权限。**在 Sirius 自己具备鉴权之前，服务只能绑 `127.0.0.1`**，不得暴露到局域网或公网
- 不代理 AMKR 的 `/ws/events`：WebUI 不用它（只用 fetch + 轮询），没必要处理升级
- AMKR 的配置文件、metrics sqlite、上游 key 都在 AMKR 那边管理，**不进本仓库**，Sirius 也不读它们

> WebUI 是随 AMKR wheel 发布的预构建静态 ES module，无构建步骤。`index.html` 用相对路径引资源，因此挂在任意前缀下都能工作 —— 这正是"不改写路径"能成立的原因。

## 5. 进程模型与部署

- v1 用 docker compose 两个容器：`amkr` + `sirius`，Sirius 通过服务名访问 `http://amkr:8000`
- 单镜像双进程是**后续可选的分发优化，不是 v1 目标**。先把链路跑通，再谈合并
- 就绪与存活判断打 AMKR 的 `/health`（该接口免鉴权），不要用 `/` 或猜端口
- AMKR 镜像的 Dockerfile、tzdata 依赖等由 AMKR 仓库那边负责，**不在本仓库处理**。Sirius 只需假设 AMKR 的 HTTP 契约可用

## 6. 已知契约要点（实现时的检查清单）

来自对 AMKR 源码与文档的核对，容易踩：

| 项 | 事实 |
|---|---|
| 鉴权 | `Authorization: Bearer <local_api_key>`，或 `x-api-key`；`/health`、`HEAD /`、`/docs` 免鉴权 |
| 代理路径 | `POST /v1/chat/completions`；另有 `/v1/messages`（Anthropic）、`/v1/responses` |
| 管理 API | 前缀 `/api/*`，需要本地 key；响应含 `config_revision`，写操作要带版本号否则 `409` |
| 服务未就绪 | 无可用 Key 时返回 `503`；上游失败返回 `502` |
| 任务路由冲突 | 任务名与模型名撞名会在**配置加载时**直接报错，不是运行时 |
| 参数冲突 | 请求里显式传了任务已固定的采样参数 → `400`（见第 2 节） |
| 切换 Key | 同一模型配了多个 Key 时由 AMKR 决定用哪个，Sirius 无法（也不需要）指定 |
