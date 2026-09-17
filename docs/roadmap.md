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

**记忆部分按 [`memory.md`](memory.md) §8 Phase 1 做**：unread 队列、`@我`/回复打断（带 `uninterruptible` guard）、已读游标、关键词打捞返回整段、记忆曲线 + Shadow、升格（源条目删除）。
**Phase 1 不做**：RAG/embedding、LLM 整合、自我模型。

### 验收标准

连续跑 30 个 tick：

1. 不自锁、不死循环
2. 每次状态转移都有结构化日志（R6）
3. LLM 调用期间状态机**不被阻塞**（R4）—— 调用中仍能收事件、仍能被抢占
4. 同一状态不连续进入两次（R3）
5. 不在 `scrolling_phone` 时 QQ 消息**不进 prompt**；被 @ 能打断；`sleeping` 时不打断但有记录
6. 有记忆因长期不打捞而沉入 Shadow，且 Shadow 内容**不出现在 prompt**

## 3. 跑通之后再考虑

按 [`memory.md`](memory.md) §8 的分期，不承诺顺序：

- **Phase 2**：LLM 整合（event → consolidated）、自我模型 + 常驻 prompt、整合记忆参与检索
- **Phase 3**：RAG（若关键词打捞被证明不够用，且 AMKR 侧 embedding 可用）
- 多个 agent 互动
- 心境真正影响权重（MVP 里心境可以只是存在但不参与）
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
| embedding / RAG | **推迟到 Phase 3** | AMKR 下个版本起原生支持 embeddings（v4.1.0 无）。Phase 1/2 用关键词 + LLM 重排，先验证机制再付向量成本。见 [`memory.md`](memory.md) §5.2 |
| 任务名划分 | 待定 | 先用一个任务跑通，之后按调用点（状态分派 / 内心独白 / 工具解读）拆 |
| Sirius 自身鉴权 | **阻塞项** | 没有它就不能把 `/amkr/` 暴露到 localhost 之外 |
| 持久化 | 暂不需要 | v1 全内存，重启即清零 |
| 前端设计风格 | 待定 | 参考 `design-taste-frontend` 技能 |
