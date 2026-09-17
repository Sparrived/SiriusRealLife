# 范围与路线

## 1. 反目标（明确不做）

这些是刻意砍掉的，不是忘了。想加先在本文档里改约定，再写代码。

- ❌ DDD 分层、聚合根、领域事件总线
- ❌ 事件溯源 / CQRS
- ❌ 插件系统、工具运行时注册表、反射扫描
- ❌ 抽象工厂、只有一个实现的接口
- ❌ 微服务、消息队列、K8s —— 单体一个进程跑起来再说
- ❌ 为"以后可能要多用户"提前做多租户
- ❌ 自建 LLM 供应商适配层 / SDK 封装（AMKR 已经做了）
- ❌ 自建管理后台（直接反代 AMKR 的 WebUI）
- ❌ 引入外部向量库 / 向量数据库 / ANN 服务（Phase 1/2 用 Go 内暴力余弦，见 [`memory.md`](memory.md) §5.2）
- ❌ 在 Sirius 里做重试、Key 轮询、配额统计（AMKR 的职责）

## 2. MVP 范围（第一版只做这些）

MVP 的 4 个状态（QQ 可见性标在括号里）：

| 状态 | QQ 消息可见 | 说明 |
|---|---|---|
| `scrolling_phone` | ✅ **看 QQ 就发生在这里** | 进入时返回最近 N 条，可用翻阅工具 |
| `idle` | ❌ | 发呆，消息静默入队 |
| `working` | ❌ | 工作，消息静默入队 |
| `sleeping` | ❌ | `uninterruptible: true`，@ 也延迟 |

- 1 个工具（`read_app`）——QQ 门控与翻阅见 [`memory.md`](memory.md) §2
- 固定 tick 循环，状态表硬编码在 Go 里
- LLM 走 AMKR，调用点先用**一个**任务名（如 `TASK_000001`），verify 通链路后再拆分
- SSE 推流转到 Vue，页面显示实时意识流 + 状态图（当前节点高亮）
- `/amkr/` 反代可用，能在 Sirius 页面上切到 AMKR WebUI 配模型

**记忆部分按 [`memory.md`](memory.md) §9 Phase 1 做**：unread 队列、`@我`/回复打断（带 `uninterruptible` guard）、已读游标、关键词打捞返回整段、记忆曲线 + Shadow、升格（源条目删除）。
**Phase 1 不做**（顺序靠后，非放弃）：向量检索、LLM 整合、自我模型。

### 验收标准

连续跑 30 个 tick：

1. 不自锁、不死循环
2. 每次状态转移都有结构化日志（R6）
3. LLM 调用期间状态机**不被阻塞**（R4）—— 调用中仍能收事件、仍能被抢占
4. 同一状态不连续进入两次（R3）
5. 不在 `scrolling_phone` 时 QQ 消息**不进 prompt**；被 @ 能打断；`sleeping` 时不打断但有记录
6. 有记忆因长期不打捞而沉入 Shadow，且 Shadow 内容**不出现在 prompt**

以上 6 条已由 [`internal/acceptance`](../internal/acceptance/acceptance_test.go) 逐条覆盖（14 条测试，装配方式与 `cmd/sirius` 一致）。另外补了几条原来没写进验收、但会悄悄坏掉的：

- **R3 可复现**：同种子跑 200 tick，状态序列必须逐 tick 一致
- **tick 真的驱动记忆**：只推 agent 的 tick（不手动调 `Store.Tick`），1000 tick 后记忆必须已沉入 Shadow
- **消息真的进记忆层**：走 `POST /events`，未读计数必须增长（`Store.Ingest` 曾经没有任何生产调用方）
- **意识流真的由 LLM 生成**：`call_count > 0` 且记录带类型（`Agent.Think` 曾经同样没有调用方）
- **意图跨状态存活**：换过状态后"打算做什么"仍在

> 后三条的由来值得记住：前端、单测、日志全都正常，界面上意识流也一直在滚动——
> 但整个系统其实只是一个状态机加四条硬编码旁白。**"看起来在动"不等于"链路是通的"。**

## 3. 跑通之后再考虑

按 [`memory.md`](memory.md) §9 的分期，不承诺顺序：

- **Phase 2**：向量库（§5.2：暴力余弦 + 两路融合，Go 内实现）、LLM 整合（event → consolidated）、自我模型 + 常驻 prompt、整合记忆参与检索
- **Phase 3**：规模优化（ANN，若暴力余弦成为瓶颈）、多 agent 互动
- 多个 agent 互动
- 昼夜节律（让睡眠收敛到夜间；现状入睡时刻在 24 小时上接近均匀，见 [`architecture.md`](architecture.md) §2.2）
- 配置热加载（状态表迁到 `config/`）
- 单镜像双进程分发

## 4. 待定

| 项 | 状态 | 说明 |
|---|---|---|
| LLM 接入 | **已定** | 统一走 AMKR 的 OpenAI 兼容接口，调用点用 `TASK_XXXXXX` 任务名。见 [`llm-amkr.md`](llm-amkr.md) |
| 进程模型 | **已定（v1）** | docker compose 两容器；单镜像双进程留作后续优化 |
| Shadow 语义 | **已定** | 存档但 LLM 不可读（可审计）。见 [`memory.md`](memory.md) §3.1 |
| 升格/整合去向 | **已定** | 升格→源条目**删除**（防重复事件记忆）；整合→源条目**进 Shadow**。见 [`memory.md`](memory.md) §3.2 |
| 升格阈值 | **已定（可调初值）** | 100 tick 内打捞 ≥3 次，或 importance ≥7 且打捞 ≥1 次。见 [`memory.md`](memory.md) §5.3 |
| 整合记忆影响行为 | **已定** | 双通道：可被检索 + 自指内容进**自我模型**常驻 prompt。见 [`memory.md`](memory.md) §6 |
| R7 工具可见性 | **已定** | 状态声明信息可见性 + 建议动作，非工具白名单。见 [`memory.md`](memory.md) §7.1 |
| 向量库 | **已定** | **Sirius 自有资产**（AMKR 只提供 embedding 计算）。Go 内暴力余弦 + 关键词两路融合，不引外部向量库；Phase 2 落地。见 [`memory.md`](memory.md) §5.2 |
| 任务名划分 | 待定 | 先用一个任务跑通，之后按调用点（状态分派 / 内心独白 / 工具解读）拆 |
| 待选区写入 | **未落地** | "翻到的内容整批写入待选区"（LLM 生成关键词 + importance）需要 `tool_read` 调用点，而工具层 `read_app` 还没做。因此真实运行中 staging 恒为空、Shadow 不增长——升格与打捞的机制本身已实现且有测试。见 [`memory.md`](memory.md) §9 的待办 |
| Sirius 自身鉴权 | **阻塞项** | 没有它就不能把 `/amkr/` 暴露到 localhost 之外。容器部署因此有一条硬约束：端口只能映射到宿主回环（见 `docker-compose.yml`） |
| 持久化 | v1 全内存 | 重启即清零，Phase 1 够用。⚠️ **Phase 2 起向量必须落盘**：它是项目资产，且重算 embedding 等于重复付费调 AMKR。见 [`memory.md`](memory.md) §5.2 |
| 前端设计风格 | **已定** | 实时观测仪表盘（非营销页）：单一强调色 + 发丝线分组 + 系统字体栈，`DESIGN_VARIANCE 6 / MOTION_INTENSITY 4 / VISUAL_DENSITY 6`。取值与理由见 [`web/README.md`](../web/README.md)，对比度由 `npm run check:contrast` 断言 |
