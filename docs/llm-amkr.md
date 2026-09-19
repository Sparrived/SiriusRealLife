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

### 结构化输出

需要模型回固定字段时（目前只有独白），用 `response_format = json_schema` + `strict: true`：

```jsonc
{
  "model": "TASK_XXXXXX",
  "messages": [{"role": "user", "content": "…"}],
  "stream": false,
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "monologue",
      "strict": true,                      // 关键：只有 strict 约束字段名
      "schema": { "type": "object", "properties": { … },
                  "required": [ … ], "additionalProperties": false }
    }
  }
}
```

**必须按"尽力而为"处理，不能当保证**：

- 用 `strict: true` 而不是 `json_object`。`json_object` 只保证"是 JSON"，**字段名仍由模型自起**（实测同一 prompt 下有的路由回 `answer`、有的回 `内心活动`），等于没约束
- **忽略 `response_format` 的路由不一定报错**，可能返回 `200` + 散文。这是比 `400` 更危险的失败模式：错误发生在 AMKR 侧，Sirius 只看到"解析不出字段"。实测 wb2api 就是这样
- 因此调用方必须能接受非 JSON 回复，并按文本兜底解析（见 [memory.md](docs/memory.md) §8.5）。**不要**因为"已经请求了 schema"就省掉兜底分支
- 请求里为 nil 时整个字段必须消失（`omitempty`）：不给不需要结构化输出的调用点带上空壳
- prompt 里同时写出字段名。schema 被忽略时，那是唯一还在起作用的约束

> 只在**确实需要按字段分类型**时才用。普通调用点（如工具结果解读）要的是一句话，加 schema 只会让模型把答案塞进它猜的字段里。

### 工具调用

状态决定走 `tools`（OpenAI function calling 形状），不是 `response_format`：

```jsonc
{
  "model": "TASK_XXXXXX",
  "messages": [{"role": "user", "content": "…"}],
  "tools": [
    { "type": "function", "function": {
        "name": "enter_state",
        "description": "换一个状态。可选：刷手机、发呆、工作",
        "parameters": { "type": "object",
          "properties": { "state": { "type": "string", "enum": ["scrolling_phone","idle","working"] }, … },
          "required": ["state","for_ticks","why"], "additionalProperties": false } } }
  ]
}
```

回复读 `message.tool_calls[]`，其中 `function.arguments` 是**字符串**（不是对象），要二次 `json.Unmarshal`。有些上游对无参调用回空串，按 `{}` 处理。

**同样按"尽力而为"处理**：

- **路由可能忽略 `tools`**（照旧返回 `200` + 散文，不报错）。实测服务器上的
  wb2api **支持 `tools`**（但忽略 `response_format`，见 deploy-local §8.1），
  因此支持与否**逐路由不同**，不能假设。`applyDecision` 必须有"一个可识别的
  调用都没有"的分支，降级为 `stay`
- 因此**不能**把安全/一致性建立在"模型一定调了工具"之上：候选清单进 `enum` 是**第一道**约束，`commitEnter` 里的 `inMenu` 复核是**第二道**，缺一不可
- 候选状态名放进 `enum`，被挡掉的状态连原因写进 `description`——模型看到"睡觉：此刻不满足进入条件"比看不到它更好，后者会让它反复尝试一个不可能的选项
- 工具**每次调用显式给出**，不做全局注册表：可见性随状态变化，全局注册表只能表达"所有调用看到同一套"
- 声明 `additionalProperties: false` 与 `required`。这和 `strict` 一样是**请求**，不是保证
- 一次回复可能同时带 `content` 与 `tool_calls`（模型边叙述边动手）。两者都要留：叙述进意识流，调用改世界
- 一次回复可能带**多个**调用：只执行**第一个**被识别的。合并执行会让"最后到底进了哪个状态"取决于遍历顺序

> ⚠️ **验证协议字段是否生效，必须设计能区分"协议生效"与"模型恰好照做"的对照。**
> 踩过两次：一次用**伪装 UA 的脚本**"验证"了 CMDC 可用（实际 AMKR 走不通）；
> 一次看到模型回了 JSON 就认为 `json_schema` 生效——其实只问它"回这三个字段"，
> prompt 自己就够了。真正的对照是**不指定字段名**，看它是否仍被约束
> （实测：没有，字段名由模型自起）。详见 deploy-local §8.0/§8.1。

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
- 所有调用必须接受可取消的 `context`：关停时取消在途调用，别让它跑完 30 秒再丢弃（对应 R4）。抢占已取消，因此**关停是唯一的取消来源**
- AMKR 可能把请求切到任意一个配了同一个模型的 Key，**响应头、字节序、错误格式都按 OpenAI 标准处理，不要依赖某个上游的私有行为**

## 4. 内嵌 AMKR WebUI

管理页面不自己写，直接反代 AMKR 自带的那套（`/ui/`）。这是**部署期约束**，实现时要守住：

- **浏览器侧地址必须是 `/amkr/ui/`（不能改），上游侧必须剥掉 `/amkr` 前缀。** 两件事都要做，理由不同：
  - 浏览器：AMKR 前端从当前页面 URL 反推 API 基址（`apiBase()` 取 `/ui/` 之前的部分），所以页面必须在 `/amkr/ui/` 下，它才会把请求发到 `/amkr/api/*`。**不能**把 `/amkr` 改写掉，否则前端会去请求 Sirius 根路径的 `/api/*`。
  - 上游：独立运行的 AMKR 在**根路径**提供服务（`/health`、`/ui/`、`/api/*`）。**实测 `/amkr/ui/index.html` 直连 AMKR 返回 404** —— 因为 `mount_app()` 是 Python 进程内挂载，Go 反代用不到它。所以转发前必须 `TrimPrefix(path, "/amkr")`。

  > ⚠️ 这里曾是本文档的一处错误：原文写"路径 1:1 透传，不改写任何段"。那只在**进程内挂载**（`mount_app`）的形态下成立，与两容器独立部署矛盾。已按实测更正，并有回归测试 `TestProxyStripsMountPrefix` 与真实服务端到端测试 `TestLiveAMKRProxy` 钉住。
  >
  > 连带结论：`apiBase()` 与"剥前缀"是**一对**，缺一不可。若用了没有 `apiBase()` 的旧版（前端发根绝对路径 `/api/*`），浏览器会把它发到 Sirius 自己，反代完全收不到。
- **必须锁定到含 `apiBase()` 的 AMKR 版本（`>= v5.2.0`），不要用 `latest`。** 镜像 tag 现在写在 **`/opt/amkr/docker-compose.yml`**（AMKR 已是独立项目，见第 5 节），不在本仓库的 compose 里。`v4.1.0` 没有这个改动，其前端把请求发到根绝对路径（`fetch("/api/settings")`），挂在 `/amkr/` 下会一律 404。`v5.2.0` 起 `apiBase()` 有了，`/amkr/` 反代**全链路实测可用**（`/amkr/health` 与带会话的 `/amkr/ui/` 管理请求均正常）。升级 AMKR 时先确认新版本的 `/ui/` 仍从页面路径推导基址，再改标签
- `Authorization: Bearer $AMKR_API_KEY` 由 Go **在服务端注入**，密钥下发给浏览器就等于泄露。注意 WebUI 自己**总是**会发 `Authorization`（首次访问时 localStorage 为空，实际值是空的 `Bearer `），因此注入必须用 **`Header.Set` 覆盖**，不能用 `Header.Add` 追加 —— Starlette 只会读**第一个** `Authorization` 头，追加时浏览器那个空凭据在前、反代注入的在后，所有管理请求都会 401
- 必须 **403 掉 `/amkr/api/logs`、`/amkr/api/tool`、`/amkr/api/service/*`、`/amkr/api/integrations/*`**。这些是"操作宿主机"的运维接口（读日志文件、启停进程、注册系统服务、改写本机 Claude Code / Codex 配置），在容器里语义不成立，而且会写脏配置。更稳的做法是启动 AMKR 时加 `--no-ops`（写入配置字段 `ops_enabled`），一次关掉全部四条路径并返回 `404`，不必逐个拉黑；关掉后 `/health` 的 `ops_enabled` 为 `false`，可直接断言
- `/amkr/` 等同于 AMKR 的完整管理权限。**在 Sirius 自己具备鉴权之前，服务只能绑 `127.0.0.1`**，不得暴露到局域网或公网
- 不代理 AMKR 的 `/ws/events`：WebUI 不用它（只用 fetch + 轮询），没必要处理升级
- AMKR 的配置文件、metrics sqlite、上游 key 都在 AMKR 那边管理，**不进本仓库**，Sirius 也不读它们

> WebUI 是随 AMKR wheel 发布的预构建静态 ES module，无构建步骤。`index.html` 用相对路径引资源，因此在任意前缀下都能加载 —— 这是"浏览器侧保留 `/amkr`"能成立的原因（与"上游剥前缀"是两件独立的事，见上）。
>
> 注入正确（`Set` 覆盖）时**不会出现本地授权页**：WebUI 启动时先打 `/health`（免鉴权），看到 `local_auth_enabled` 为真就请求一次 `/api/settings` 探活，而这个请求会被反代覆盖成有效凭据，于是直接进主界面。如果看到验证页要 Key，说明注入没生效或用了 `Add` 追加（见上）。


## 5. 进程模型与部署

- **AMKR 是独立的 compose 项目**（`/opt/amkr/docker-compose.yml`，项目名 `amkr`），不再和 Sirius 同项目。
  为什么拆：AMKR 的升级节奏、镜像来源、状态（配置 + metrics 库）都与 Sirius 无关，绑在一个项目里
  会让「只升 AMKR」变成动整个 stack，两者的生命周期也被绑死。
- 连通方式：两者共用一个独立网络 `amkr-net`，AMKR 在该网络上**保留别名 `amkr`**，因此
  `AMKR_BASE_URL=http://amkr:8000` 不用改。网络由 AMKR 那个项目创建，本仓库的 compose 以
  `external: true` 引用它 —— 在 Sirius 目录里 `down` 不会拆掉 AMKR 的网络。
- 代价：跨项目的 `depends_on: service_healthy` 不再成立（compose 的 `depends_on` 只在同一项目内
  生效），Sirius 可能比 AMKR 先起来，启动瞬间的调用会失败、之后自愈。要消除这个窗口就按顺序起：
  `cd /opt/amkr && docker compose up -d` → 等 `/health` 返回 200 → 再起 sirius。
  两份 compose 都写死了 `name:`，所以**不要**加 `-p`：`docker compose -p amkr down` 在任何
  目录执行都会按项目标签拆掉 amkr 的容器，但服务定义取自当前目录那份文件 —— 实测在
  SiriusRealLife 目录里执行会起出一个用 sirius 定义的 `amkr-sirius-1`，同时把真正的
  `amkr-amkr-1` 停掉删除（还会抢 18080 端口）。`cd` 到正确目录再执行，只用 `name:`。
- 单镜像双进程是**后续可选的分发优化，不是当前目标**。先把链路跑通，再谈合并
- 就绪与存活判断打 AMKR 的 `/health`（该接口免鉴权），不要用 `/` 或猜端口
- **锁死镜像 tag**，不要用 `latest`：`/amkr/` 反代依赖前端的 `apiBase()` 行为，而该行为在版本之间变过
- AMKR 容器启动参数是否带 `--no-ops`（见第 4 节）在 `/opt/amkr/docker-compose.yml` 里定。注意
  `v5.2.0` **已支持** `--no-ops`（实测该 flag 被接受，未知 flag 会报 `flag provided but not defined`），
  所以「等它发布后再加」这个历史约束已经解除
- AMKR 镜像的 Dockerfile、tzdata 依赖等由 AMKR 仓库那边负责，**不在本仓库处理**。Sirius 只需假设 AMKR 的 HTTP 契约可用

### 5.1 AMKR 必须自己配好 provider（否则每次调用都 500）

**空配置的 AMKR 能启动、`/health` 返回 `ready: true`，但每一次 LLM 调用都失败。** 这不是"没配好但还能降级"，而是**上线即全挂**，且症状极具误导性：Sirius 侧只看到解析错误，真正的原因在 AMKR 容器里。

实测过一次完整的坑：服务器上 AMKR 的 `providers` 与 `models` 都是空的、`unified_model` 为 `null`，于是请求 `model: "unified-model"`（Sirius 的默认值）时，AMKR 直接抛 `KeyError: 'unified-model'` 并返回 `500`。

**部署 AMKR 后必须做完这三步，缺一不可**：

| 步骤 | 配置项 | 说明 |
|---|---|---|
| 1 | `providers.<名>` | 上游地址 + Key。Key 放在 `keys.<名>.api_key`，同时给 `enabled: true` |
| 2 | `models.<名>` | 模型条目，`targets[]` 指回 provider 与**上游真实模型名** |
| 3 | `unified_model.default.primary.model` | 指向第 2 步的**条目名**。Sirius 传的 `model` 通常就是 `unified-model`，它靠这里解析 |

**排障只看一处**：`/data/auto-model-key-router/server.log`（在 AMKR 容器/卷里）。`/health` 的 `ready: true` **不代表**有可用模型，两者是独立的。

> `server.log` 不在宿主的 `/tmp` 可见范围里，容器有独立文件系统。
>
> 症状对照：`500` + 日志里 `KeyError: 'unified-model'` = 第 3 步没做；`503` = 有模型但 Key 全不可用。

**provider 指向宿主端口时**：容器访问宿主已发布端口需要 `extra_hosts: ["host.docker.internal:host-gateway"]`（compose 里已加）。Linux 上该名字不会自动存在，不声明就只能写死 `172.17.0.1`——而那是默认 bridge 的网关，换网络模式即失效。

> 实测记录：`127.0.0.1:<port>` 在**容器内**指向容器自己，连不通宿主；公网域名可能被 Cloudflare 按 UA 拦（`403 error code: 1010`，那是边缘拦截而非上游鉴权失败，别据此判断 Key 有问题）。

## 6. 已知契约要点（实现时的检查清单）

来自对 AMKR 源码与文档的核对，容易踩：

| 项 | 事实 |
|---|---|
| 鉴权 | `Authorization: Bearer <local_api_key>`，或 `x-api-key`；免鉴权的确切集合是 `/health`、`HEAD /`、`/docs`、`/openapi.json`、`/redoc`。后三个会暴露全部路由、参数与 schema，反代到公网前要一起挡掉。另外只认**第一个** `Authorization` 头（见第 4 节） |
| 代理路径 | `POST /v1/chat/completions`；另有 `/v1/messages`（Anthropic）、`/v1/responses` |
| 管理 API | 前缀 `/api/*`，需要本地 key；响应含 `config_revision`，写操作要带版本号否则 `409` |
| 运维 API | `/api/logs`、`/api/tool`、`/api/service/*`、`/api/integrations/*`，默认**开启**且同样只需本地 key。容器里用 `--no-ops` 关掉（见第 4 节）；只靠反代拉黑时四条都要覆盖 |
| 服务未就绪 | 无可用 Key 时返回 `503`；上游失败返回 `502` |
| 只配 `unified_model` 不够 | 还必须配 `providers` 与 `models`，否则请求 `unified-model` 抛 `KeyError` → `500`（见 §5.1） |
| `ready: true` ≠ 有模型 | `/health` 只表示进程就绪。空配置也是 `ready: true`，但每次调用都 500 |
| `response_format` | 可传 `json_schema`；**忽略它的路由不一定报错**，可能 200 + 散文（见 §2「结构化输出」） |
| 任务路由冲突 | 任务名与模型名撞名会在**配置加载时**直接报错，不是运行时 |
| 参数冲突 | 请求里显式传了任务已固定的采样参数 → `400`（见第 2 节） |
| 容器 | 镜像 `ghcr.io/sparrived/auto-model-key-router`（tag 为版本号，正式版另带 `latest`）。容器内固定监听 `0.0.0.0`（否则端口映射进不去），端口默认 8000，状态在卷 `/data`（配置为 `/data/auto-model-key-router/router-config.json`）。端口**只发布到宿主回环**（`127.0.0.1:28881:8000`），供 Cloudflare Tunnel 反代，见下 |
| 取本地 key | `cd /opt/amkr && docker compose exec amkr amkr --config /data/auto-model-key-router/router-config.json --get-key`。该命令会直接输出完整凭据，别在共享终端或会记录历史的地方跑 |

### 当前部署的实际状态（v5.2.0，2026-09-19 拆分后）

- `/amkr/` 反代**已全链路可用**：上游剥前缀 + `Header.Set` 注入 + 运维路径 403 都生效，
  浏览器侧保留 `/amkr` 让前端的 `apiBase()` 把请求发到 `/amkr/api/*`。
- AMKR 是独立项目，容器 `amkr-amkr-1`，镜像 `ghcr.io/sparrived/auto-model-key-router:5.2.0`
  （**该 GHCR 包已公开**，可匿名拉取）。数据在**无 compose 项目标签**的卷 `amkr-data` 上。
- 对外的三条路径（都已实测 200）：宿主回环 `http://127.0.0.1:28881`、容器间
  `http://amkr:8000`、公网 `https://amkr.sparrived.xyz`（Cloudflare Tunnel → 宿主 28881）。
- ⚠️ **公网那条等同于 AMKR 的完整管理权限**（能读上游 key、能改全部配置）。它靠 AMKR 自身的
  `local_api_key` 挡住（匿名访问 `/api/*` 实测 401）。主 WebUI 的 Key 存在浏览器 localStorage；
  嵌入式工作空间面板则把 Key 放在 URL **fragment** 里（`#k=...`，fragment 不发给服务端，见
  AMKR 仓库的 docs/PANEL.md）。若要进一步收紧，在 Cloudflare 侧给该 hostname 加 Access 策略。
- ⚠️ AMKR 的**运维 API 仍是开启的**（`/health` 的 `ops_enabled` 为 `true`）。Sirius 的 `/amkr/`
  反代会 403 掉这几条，但**经宿主 28881 / 公网访问时没有这层保护**，只有 local key 一道鉴权。
  不想要这个暴露面就给 `/opt/amkr` 的启动参数加 `--no-ops`（v5.2.0 支持）。代价仅限 WebUI
  设置页：工具信息探针有 `.catch(() => null)` 会静默降级为「读不到」，服务操作按钮则会显示
  「服务操作失败」——页面本身照常打开。

| 切换 Key | 同一模型配了多个 Key 时由 AMKR 决定用哪个，Sirius 无法（也不需要）指定。走了备选模型时响应带 `X-AMKR-Fallback: true`，可用于观测降级 |
