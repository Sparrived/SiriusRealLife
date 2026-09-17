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

- **浏览器侧地址必须是 `/amkr/ui/`（不能改），上游侧必须剥掉 `/amkr` 前缀。** 两件事都要做，理由不同：
  - 浏览器：AMKR 前端从当前页面 URL 反推 API 基址（`apiBase()` 取 `/ui/` 之前的部分），所以页面必须在 `/amkr/ui/` 下，它才会把请求发到 `/amkr/api/*`。**不能**把 `/amkr` 改写掉，否则前端会去请求 Sirius 根路径的 `/api/*`。
  - 上游：独立运行的 AMKR 在**根路径**提供服务（`/health`、`/ui/`、`/api/*`）。**实测 `/amkr/ui/index.html` 直连 AMKR 返回 404** —— 因为 `mount_app()` 是 Python 进程内挂载，Go 反代用不到它。所以转发前必须 `TrimPrefix(path, "/amkr")`。

  > ⚠️ 这里曾是本文档的一处错误：原文写"路径 1:1 透传，不改写任何段"。那只在**进程内挂载**（`mount_app`）的形态下成立，与两容器独立部署矛盾。已按实测更正，并有回归测试 `TestProxyStripsMountPrefix` 与真实服务端到端测试 `TestLiveAMKRProxy` 钉住。
  >
  > 连带结论：`apiBase()` 与"剥前缀"是**一对**，缺一不可。若用了没有 `apiBase()` 的旧版（前端发根绝对路径 `/api/*`），浏览器会把它发到 Sirius 自己，反代完全收不到。
- **必须锁定到含 `apiBase()` 的那个 AMKR 版本**（写入 `docker-compose.yml` 的 `image:` 标签，不要用 `latest`）。`v4.1.0`（当前正式版）**没有**这个改动，它随下一个版本发布；接入时填那个 tag。旧版前端用根绝对路径（`fetch("/api/settings")`），挂在 `/amkr/` 下会一律 404 —— 即前端发出的请求根本不会经过 `/amkr/`，反代无从生效。升级 AMKR 时先确认新版本的 `/ui/` 仍从页面路径推导基址，再改标签
- `Authorization: Bearer $AMKR_API_KEY` 由 Go **在服务端注入**，密钥下发给浏览器就等于泄露。注意 WebUI 自己**总是**会发 `Authorization`（首次访问时 localStorage 为空，实际值是空的 `Bearer `），因此注入必须用 **`Header.Set` 覆盖**，不能用 `Header.Add` 追加 —— Starlette 只会读**第一个** `Authorization` 头，追加时浏览器那个空凭据在前、反代注入的在后，所有管理请求都会 401
- 必须 **403 掉 `/amkr/api/logs`、`/amkr/api/tool`、`/amkr/api/service/*`、`/amkr/api/integrations/*`**。这些是"操作宿主机"的运维接口（读日志文件、启停进程、注册系统服务、改写本机 Claude Code / Codex 配置），在容器里语义不成立，而且会写脏配置。更稳的做法是启动 AMKR 时加 `--no-ops`（写入配置字段 `ops_enabled`），一次关掉全部四条路径并返回 `404`，不必逐个拉黑；关掉后 `/health` 的 `ops_enabled` 为 `false`，可直接断言
- `/amkr/` 等同于 AMKR 的完整管理权限。**在 Sirius 自己具备鉴权之前，服务只能绑 `127.0.0.1`**，不得暴露到局域网或公网
- 不代理 AMKR 的 `/ws/events`：WebUI 不用它（只用 fetch + 轮询），没必要处理升级
- AMKR 的配置文件、metrics sqlite、上游 key 都在 AMKR 那边管理，**不进本仓库**，Sirius 也不读它们

> WebUI 是随 AMKR wheel 发布的预构建静态 ES module，无构建步骤。`index.html` 用相对路径引资源，因此在任意前缀下都能加载 —— 这是"浏览器侧保留 `/amkr`"能成立的原因（与"上游剥前缀"是两件独立的事，见上）。
>
> 注入正确（`Set` 覆盖）时**不会出现本地授权页**：WebUI 启动时先打 `/health`（免鉴权），看到 `local_auth_enabled` 为真就请求一次 `/api/settings` 探活，而这个请求会被反代覆盖成有效凭据，于是直接进主界面。如果看到验证页要 Key，说明注入没生效或用了 `Add` 追加（见上）。


## 5. 进程模型与部署

- v1 用 docker compose 两个容器：`amkr` + `sirius`，Sirius 通过服务名访问 `http://amkr:8000`
- 单镜像双进程是**后续可选的分发优化，不是 v1 目标**。先把链路跑通，再谈合并
- 就绪与存活判断打 AMKR 的 `/health`（该接口免鉴权），不要用 `/` 或猜端口
- AMKR 容器启动参数带上 `--no-ops`（见第 4 节），并在 compose 里**锁死镜像 tag**，不要用 `latest`：`/amkr/` 反代依赖前端的 `apiBase()` 行为，而该行为在版本之间变过
- AMKR 镜像的 Dockerfile、tzdata 依赖等由 AMKR 仓库那边负责，**不在本仓库处理**。Sirius 只需假设 AMKR 的 HTTP 契约可用

## 6. 已知契约要点（实现时的检查清单）

来自对 AMKR 源码与文档的核对，容易踩：

| 项 | 事实 |
|---|---|
| 鉴权 | `Authorization: Bearer <local_api_key>`，或 `x-api-key`；免鉴权的确切集合是 `/health`、`HEAD /`、`/docs`、`/openapi.json`、`/redoc`。后三个会暴露全部路由、参数与 schema，反代到公网前要一起挡掉。另外只认**第一个** `Authorization` 头（见第 4 节） |
| 代理路径 | `POST /v1/chat/completions`；另有 `/v1/messages`（Anthropic）、`/v1/responses` |
| 管理 API | 前缀 `/api/*`，需要本地 key；响应含 `config_revision`，写操作要带版本号否则 `409` |
| 运维 API | `/api/logs`、`/api/tool`、`/api/service/*`、`/api/integrations/*`，默认**开启**且同样只需本地 key。容器里用 `--no-ops` 关掉（见第 4 节）；只靠反代拉黑时四条都要覆盖 |
| 服务未就绪 | 无可用 Key 时返回 `503`；上游失败返回 `502` |
| 任务路由冲突 | 任务名与模型名撞名会在**配置加载时**直接报错，不是运行时 |
| 参数冲突 | 请求里显式传了任务已固定的采样参数 → `400`（见第 2 节） |
| 切换 Key | 同一模型配了多个 Key 时由 AMKR 决定用哪个，Sirius 无法（也不需要）指定。走了备选模型时响应带 `X-AMKR-Fallback: true`，可用于观测降级 |
