# 同类项目调研（可借鉴的逻辑）

调研目的：在动手写状态机之前，确认**哪些设计别人已经趟过、哪些参数可以直接抄、哪些是已知会翻车的地方**。

## 0. 方法与局限

- **本会话 `web_search` 不可用**（DeepSeek 搜索端点 402 余额不足），所有材料来自直接抓取 + GitHub API 搜索。
- 因此覆盖**不是系统性的**：以公认的经典项目为锚点（Stanford Generative Agents、DeepMind Concordia），再用 GitHub API 按主题补搜。可能存在遗漏。
- 每条结论都标了出处。带 ⚠️ 的是**我的推断**，不是原文结论。
- **量化常量已逐字核对**（对 arXiv 全文，非摘要）。核对结果见 §1.5。

---

## 1. 必读：Generative Agents（Stanford, UIST'23）

[论文](https://arxiv.org/abs/2304.03442) · [代码](https://github.com/joonspk-research/generative_agents) · 全文细节取自 [ar5iv 版](https://ar5iv.labs.arxiv.org/html/2304.03442)

这是这个领域的奠基工作，也是**你的设计与它差异最大的地方**。它同样把"自觉行为"拆成三块：**记忆流（memory stream）+ 反思（reflection）+ 计划（planning）**。

### 1.1 它对你的设计最直接的一句话

论文 Related Work 明确说：基于规则的做法（**有限状态机**、行为树）是"人工编写 agent 行为的蛮力做法"，"**至今仍是最主流的方法**"，但——

> 手工构造能覆盖开放世界全部交互可能性的行为是**不可持续的**（untenable）……并且**无法执行任何没有硬编码进脚本的新过程**。

同时它也说了认知架构（SOAR / ICARUS）的教训：维护短期与长期记忆、跑 perceive-plan-act 循环，但"**行动空间受限于人工编写的程序性知识**，没有机制让 agent 去寻求新行为"。

⚠️ **对你的意义**：你的"状态 = 工具集合 + 内心状态 + 事件触发"本质上就是这条线的现代版。论文没有否定它，而是指出**单靠它不够**——必须再叠一层"记忆检索 + 反思"，否则人格不会从经历里长出来，只会按权重表循环。这正好对应我们上次讨论的"第 20 轮人格被磨平"。

### 1.2 检索函数：直接可抄的公式

记忆对象的结构：**自然语言描述 + 创建时间戳 + 最近一次访问时间戳**。

检索分数由三项加权（论文里三个 α **全设为 1**，各项先 min-max 归一化到 [0,1]）：

```
score = α_recency · recency + α_importance · importance + α_relevance · relevance
```

| 项 | 实现 | 可抄的细节 |
|---|---|---|
| **recency** | 距上次检索的游戏小时数的指数衰减 | **衰减系数 0.995** |
| **importance** | 创建时让 LLM 打分 | 1–10，"刷牙/铺床"=2，"约暗恋对象出去"=8 |
| **relevance** | 记忆描述的 embedding 与查询的余弦相似度 | 需要一个 embedder |

importance 的原文 prompt：

> On the scale of 1 to 10, where 1 is purely mundane (e.g., brushing teeth, making bed) and 10 is extremely poignant (e.g., a break up, college acceptance), rate the likely poignancy of the following piece of memory.

⚠️ **对你的意义**：我上次说你的意识流"三层够用"，这个判断要修正——**你缺的是 working 层里的检索函数**。只有 `最近 N 条` 是不够的：最近 N 条会漏掉"前天那件很重要的事"。上面这个三因子公式是现成的，且 paper 做了消融实验证明有效。

⚠️ **注意成本**：relevance 需要 embedding。AMKR 下一个版本起原生提供 embedding **计算**（`v1/embeddings`），但向量**库**（索引、持久化、余弦检索、融合排序）是 Sirius 自己的活，不能省。v1 可以先只用 **recency + importance**，把 relevance 留到 Phase 2 —— 顺序问题，不是取舍。**这比不做检索、只取最近 N 条要好。**

### 1.3 反思（reflection）：你缺的这一层

论文的做法，是**触发条件 + 两段式生成**：

1. **触发**：当最近感知事件的重要度**累加超过 150** 时触发（实践中约**每天 2–3 次**）
2. 取**最近 100 条**记录，问 LLM："Given only the information above, what are 3 most salient high-level questions we can answer about the subjects in the statements?"
3. 用生成的 3 个问题**作为检索查询**，取回相关记忆
4. 再问："What 5 high-level insights can you infer from the above statements? (example format: insight (because of 1, 5, 3))"
5. 把 insight 存回记忆流，**带指向被引用记录的指针**

关键性质：反思可以**基于反思再做反思**，于是长成一棵**反思树**——叶子是观察，非叶节点越往上越抽象（论文 Figure 7：Klaus 从"在写研究论文/在读中产化社区的书"这些叶子，推出"Klaus 对研究非常投入"）。检索时反思和观察**平等参与**。

⚠️ **对你的意义**：你的 `consolidated`（整合记忆）就是这一层，但它解释了为什么需要、以及**什么时候压**：论文的例子是——只给原始观察时，问 Klaus"你最想和谁待一小时"，他选了**见面最频繁但并不亲近**的宿舍邻居 Wolfgang；有了反思层，他选了**真正有共同兴趣**的 Maria。触发条件与两段式流程已并入 [`memory.md`](memory.md) §4/§6。

**这是"人格"和"聊天记录回放"的分界线。**

### 1.4 计划（planning）：与"随机分派"的关系

计划的结构：**地点 + 开始时间 + 时长**。计划本身也存进记忆流，参与检索。

生成方式是**自顶向下递归分解**：先出一天的大纲（**5–8 块**），再拆到小时级，再拆到 **5–15 分钟**级。

论文给的动机例子非常值得记住：

> 如果直接问 LLM"此刻 Klaus 该做什么"，他会 12 点吃午饭，然后 12:30 又吃一次、1 点再吃一次——**为当下的可信度牺牲了时间尺度上的可信度。**

⚠️ **对你的意义**：这是**你那个"随机分派器"最该被质疑的地方**。纯加权抽取只保证"这一步看起来合理"，不保证"这条轨迹像个人"。论文的解法是**计划层 + 随时重规划（re-planning）**。

⚠️ **我的建议**：v1 不需要完整的递归计划器，但至少要有**一个粗粒度的当日意图**（哪怕就是"今天想干什么"一句话，每天生成一次），让状态分派**在意图的约束下**加权抽取。这样成本极低，却能拦住"吃三次午饭"这类问题。

### 1.5 常量核对结果（已对全文逐字核实）

初稿里几个数值是凭既有印象写的，**现已全部对 arXiv 全文核对，结论：七处全部正确**。以下为原文依据，可直接作为实现时的初始值。

| 常量 | 值 | 原文依据 |
|---|---|---|
| recency 衰减系数 | **0.995** | "Our decay factor is 0.995" |
| recency 度量 | **游戏内小时**，自**上次被检索**起算 | "exponential decay function over the number of sandbox game hours since the memory was last retrieved" |
| importance 量程 | **1–10 整数**，由 LLM 生成，**创建记忆时打分** | "returns an integer value of 2 for 'cleaning up the room' and 8 for 'asking your crush out on a date'"；"generated at the time the memory object is created" |
| 三项权重 α | **全部 = 1** | "all αs are set to 1" |
| 归一化方式 | **min-max 缩放到 [0,1]** | "normalize … to the range of [0,1] using min-max scaling" |
| 反思触发阈值 | **重要度和 > 150** | "exceeds a threshold (150 in our implementation)" |
| 反思取用条数 | **最近 100 条** | "we query the large language model with the 100 most recent records" |
| 反思产出 | **3 个问题 → 5 条洞见**，洞见须**引用来源记录** | "what are 3 most salient high-level questions"；"What 5 high-level insights … (example format: insight (because of 1, 5, 3))"；"cite the particular records that served as evidence" |
| 反思频率（观测值） | 约**每天 2–3 次** | "our agents reflected roughly two or three times a day" |
| 计划层级 | 大纲 **5–8 块** → 小时级 → **5–15 分钟**级 | "divided into five to eight chunks"；"recursively decompose this again into 5–15 minute chunks" |

原文出处（arxiv.org/html/2304.03442v2）：recency/importance/relevance 三项及其公式见 §4.1；反思阈值、条数、两段式 prompt 见 §4.2；计划的递归分解见 §4.3。

**一处需澄清**：论文说 recency 按"**自上次被检索以来**"的小时数衰减（`since the memory was last retrieved`），不是自创建以来。这意味着**检索行为本身会刷新 recency** —— 被反复想起的事会更持久，这正是 [`memory.md`](memory.md) 讨论"打捞/遗忘"时要对齐的机制，且与仿生记忆曲线同源。

---

### 1.6 已核实的失败模式（可作为验收清单）

论文原文（§6 讨论）列出三类最常见错误：

> the most common errors arose when the agent **failed to retrieve relevant memories**, **fabricated embellishments** to the agent's memory, or **inherited overly formal speech or behavior** from the language model.

并有一条脚注解释第三类：这种过分正式的语气**很可能来自底座模型的 instruction tuning**，属于模型特性而非架构缺陷 —— "We expect that the writing style will be better controllable in future language models."

⚠️ **对你的意义**：第三条是**可以通过人格设定压制**的（在 system prompt 里给定语言习惯、口头禅、句长），不该指望它自己好转。前两条才是架构问题。

### 1.7 消融与伦理

- **消融**：观察、计划、反思**三者各自都关键**（removing any one degrades believability）——不是可选项。
- **伦理提醒**（论文自己强调，你如果公开部署要留意）：应调优以**降低用户产生准社会关系（parasocial）的风险**、应**记录日志**以缓解 deepfake 与定向说服风险。

---

## 2. DeepMind Concordia：解决"状态触发事件"的正统做法

[代码](https://github.com/google-deepmind/concordia) · [技术报告 arXiv:2312.03664](https://arxiv.org/abs/2312.03664) · [设计模式论文 arXiv:2507.08892](https://arxiv.org/abs/2507.08892)

它把自己定位成生成式 agent 的**游戏引擎**，三个核心概念：

| 概念 | 作用 |
|---|---|
| **Entities** | 模拟中的行动者：玩家角色（Agents）或系统控制者（Game Masters） |
| **Components** | Entity 的模块化积木：登录、思维链、记忆操作……都在 Component 里 |
| **Engine** | 模拟循环：**向各 Entity 征求动作，把裁决交给 Game Master** |

**Game Master（GM）模式**是重点，也是直接回答你"通过状态触发事件"的那一块：

> Entities 用**自然语言描述打算做的动作**，GM 把这些翻译成结果，例如在模拟世界里**检查物理可行性**。

⚠️ **对你的意义**：你现在的设计是"状态触发事件"，但**谁判定事件是否成立、结果是什么**——这个角色你没定义。Concordia 的答案是引入一个**独立的 GM 实体**做裁决器。对你的好处：

- 状态只管"我想干什么"，GM 管"这么干会怎样"，**两者可以独立演进和测试**
- GM 是**唯一**知道世界状态的地方，状态机不需要读世界

另外注意：Concordia **需要一个 text embedder** 做联想记忆——和 Generative Agents 一样，说明 embedding 在这类系统里近乎标配。

它 README 里的示例也值得一提：4 个朋友被雪困在酒吧，两人为撞坏的车争吵。agent 用的是 March & Olsen (2011) 的三个问题——

1. What kind of situation is this?
2. What kind of person am I?
3. What does a person such as I do in a situation such as this?

⚠️ 这个三段式**正好是"状态"与"心境"分离的理论表述**：第 1 问是状态（情境），第 2 问是人格/心境，第 3 问才是行为。可以直接借来当 prompt 骨架。

---

## 3. FSM × LLM 的工程实践（三个不同侧重）

这条线上已经有现成项目，而且**踩过的坑不一样**，正好互补。

### 3.1 [jsz-05/LLM-State-Machine](https://github.com/jsz-05/LLM-State-Machine)（PyPI `fsm-llm`）

用装饰器声明状态，**转移条件写成自然语言，由 LLM 判断该跳哪个**：

```python
@fsm.define_state(
    state_key="START",
    prompt_template="You are an on-off switcher. Ask the user if they want to turn the switch on or off.",
    transitions={"STATE_ON": "If user wants to turn on the switch", "END": "If user wants to end the conversation"},
)
```

⚠️ **两点观察**：
- 它把转移决策**交给 LLM**，你是**交给权重抽取**。各有代价：LLM 判定更灵活但每次转移都要一次调用（贵、慢、不稳定）；权重抽取免费可复现但不会"审时度势"。**混合方案**可能最优：权重决定候选集，LLM 只在候选集里挑（或反之）。
- 它是**对话驱动**的（`while not fsm.is_completed(): input(...)`），**没有自主 tick**。它解决的是"按流程走完一个客服对话"，不是"没人理的时候自己待着"。这恰好是你的场景**没有现成答案**的部分——**自主性是你的增量**。

### 3.2 [xforce-io/milkie](https://github.com/xforce-io/milkie)（TS）

一句话概括："agents are finite-state machines and **every run is traceable, replayable, and forkable**"。

⚠️ 这直接印证了 AGENTS.md 里的 **R6（每次转移都留结构化日志）** 和 **R8（只认 tick 序号）**——可回放/可分叉是这类系统的**一等需求**，不是调试附属品。我们的 R6 已经要求记 `from/to/reason/权重快照/随机数`，加上 R8 的确定性 tick，**回放和分叉是免费拿到的**。建议在 MVP 验收里真的加一条"能回放一段轨迹"。

### 3.3 [msradam/phoebe](https://github.com/msradam/phoebe)（SRE 事故调查 FSM over MCP）

**这条对你的设计是一记直接反驳，必须看：**

> The agent keeps the full Grafana toolset; **the FSM gates the procedure (triage, diagnose, verify, conclude) and the audit trail, not the tools.**

⚠️ **这是对你"状态本质是一个工具集合"的正面挑战**。它的经验是：**不要把工具可见性绑在状态上**——让 agent 始终持有完整工具集，FSM 只约束**流程**和**审计**。

它的理由（⚠️ 我的推断，但逻辑成立）：如果把工具和状态绑死，那么"能不能用某个工具"就变成了硬编码的路由判断，**状态一多就会组合爆炸**，而且挡住模型在意外情境下使用正确工具。

**我的判断**：两种做法都有正当场景，但**你该默认走 phoebe 那条**：

- 状态声明 `tools` 作为**建议**（进 prompt 提示"现在适合做什么"），而不是**硬白名单**
- 只在**确实有安全/一致性理由**时才硬封锁（例如"睡觉时不能发消息"）
- 否则你会很快遇到"状态 A 需要 read_app，状态 B 也需要，状态 C 也需要……"的复制粘贴

AGENTS.md 目前的 **R7** 写的是"状态声明所需工具写 `Tools: []string{"read_app"}`"，措辞上是白名单。**建议明确改成"建议 + 例外硬封锁"。**

---

## 4. 工程教训：[a16z-infra/ai-town](https://github.com/a16z-infra/ai-town)

可部署的 starter kit（JS/TS + Convex）。它的价值不在架构创新，而在**把这类系统真的跑起来之后会遇到什么**：

| 教训 | 细节 |
|---|---|
| **引擎与 agent 分离** | 用 Convex 的 shared global state + transactions + 独立 simulation engine；agent 行为跑在 engine 里，不由前端驱动 |
| **记忆检索条数是个性能旋钮** | 有 `NUM_MEMORIES_TO_SEARCH` 常量，README 明说"如果觉得慢就调小"——**说明检索条数直接决定 prompt 体积和延迟** |
| **必须能暂停世界** | 窗口空闲 5 分钟自动暂停；也能手动 freeze/unfreeze；还能 `stop`/`resume`/`kick` engine、`archive` 整个世界 |
| **换 embedding 模型要清库** | 向量维度必须匹配，音 README 明确警告改模型就得 wipe 数据 |
| **本地推理要处理网络问题** | Ollama 在容器里访问宿主机需要 `host.docker.internal` 或 socat 桥接——**和我们的 compose 方案同构** |

⚠️ **对你的意义**：
- "引擎与 agent 分离"对应我们的 **R1**（agent 独占自己的 goroutine）——**方向一致，被验证过**。
- "能暂停/存档/冷启动"是 v1 就该留的接口位（哪怕只是 `POST /api/v1/agents/{id}/pause`），否则调试 30 tick 验收时会很痛苦。
- `NUM_MEMORIES_TO_SEARCH` 这个旋钮提醒：**检索条数要可配置**，别写死。

---

## 5. 其他值得扫一眼的实现

| 项目 | 一句话 |
|---|---|
| [mgarasz/llm-memory-stream](https://github.com/mgarasz/llm-memory-stream) | 只实现记忆流 + 反思两个操作的最小实现（8★，适合当参照读） |
| [QuangBK/generativeAgent_LLM](https://github.com/QuangBK/generativeAgent_LLM) | 用 LangChain + Guidance 复现论文，**支持本地 LLM**（287★） |
| [sethkarten/LLM-Economist](https://github.com/sethkarten/LLM-Economist) | 2025 年，多 agent 生成式模拟 + 机制设计（MIT，124★），看"多 agent"方向 |
| [grahamhome/llm-ant-farm](https://github.com/grahamhome/llm-ant-farm) | 本地 LLM 版复现（60★） |
| [horenbergerb/llamagotchi](https://github.com/horenbergerb/llamagotchi) | LLaMA 上的复现实验（22★） |
| [LeoKwo/unscripted](https://github.com/LeoKwo/unscripted) | "LLM-powered digital human game"，体量小但方向最接近"数字人" |
| [ajmalik56/...Local-AI-Simulation...](https://github.com/ajmalik56/Generative-Agents-Local-AI-Simulation-of-Human-Behavior) | 主打**低成本**本地可跑 |

---

## 6. 结论：已采纳的修正

调研的产出已落到 [`memory.md`](memory.md) 与 [`roadmap.md`](roadmap.md)。下表是**去向**，不再是待议事项。

### 6.1 已采纳

| # | 结论 | 落地位置 |
|---|---|---|
| 1 | 记忆需**检索函数**（recency 0.995 衰减 + importance 1–10 打分） | `memory.md` §4/§5：关键词打捞 + 记忆曲线；importance 用于升格对冲 |
| 2 | **遗忘要有触发条件**，不能只是"上下文爆了才裁" | `memory.md` §4：三层机制（曲线衰减 / LLM 整合 / 重要性淘汰） |
| 3 | 状态**不再声明工具白名单** | `memory.md` §7.1；AGENTS.md R7 已修订 |
| 4 | 纯加权分派无法保证**时间尺度一致** | 暂不引入计划层（MVP 收敛），风险见 §6.3 |
| 5 | "状态触发事件"需要**裁决者** | 暂不引入 GM 角色（MVP 单 agent 无世界交互），多 agent 时再做 |

### 6.2 已写进验收标准

论文自报的三类失败模式（§1.6）作为**人工检查项**（跑完 30 tick 后看一遍）：

1. 有没有**取不到相关记忆**（该想起的没想起）
2. 有没有**编造没发生过的事**
3. 有没有**说话越来越像客服**（继承 LLM 的正式腔）——可由人格设定压制

另加 ai-town 的一条：**能暂停、能存档、能回放**（§4）。

### 6.3 明确不做（及风险）

- **不自建计划层 / 每日意图**：MVP 收敛掉。⚠️ **已知风险**：纯加权分派可能产生"12 点吃午饭、12:30 又吃、1 点再吃"这类时间尺度不一致的行为（§1.4）。若 Phase 1 验收时观察到，补一个每日粗粒度意图即可。
- **不引入 GM/裁决器**：单 agent、无世界交互，暂不需要。多 agent 互动时（roadmap §3）再加。
- **向量库要做，不是可选项**：见 `memory.md` §5.2。AMKR 只提供 embedding **计算**，索引/持久化/检索/融合排序是 Sirius 的自有资产（Phase 2 落地）。自建部分**不引外部向量库**——1 个 agent 用暴力余弦足够，符合反目标。⚠️ 必须防 ai-town 那类坑：**换 embedding 模型要重建索引**，记录里存模型标识。⚠️ 风险：纯关键词阶段会漏同义词；补偿手段是 LLM 查询扩展。
- **不引入 Concordia / LangChain 之类框架**：架构思路照抄，代码自己写（符合 AGENTS.md 反目标）。
- **不自建计划器的递归分解**：那是为 25 个 agent 跑两天的研究项目准备的，1 个 agent 用不上。

### 6.4 这套设计里**没有现成答案**的部分

项目的真正增量，没有经验可抄：

1. **自主性 + 随机分派**。所有 FSM×LLM 项目（`fsm-llm`、`phoebe`、`milkie`）都是**外部输入驱动**的：用户说话、告警触发。**"没人理的时候自己决定下一秒干什么"几乎没有现成参考**，只有 Generative Agents 的 plan/reflect 循环沾边。
2. **心境与自我模型的双时间尺度**。论文用 plan + importance 间接实现了类似效果，但没有把"短期心境"与"长期自我认同"抽成两个独立连续量。⚠️ 没有经验可抄，要自己调。
3. **注意力门控**（`memory.md` §2）。论文的感知是环境推送，没有"我不想看"这个维度。agent 主动决定何时看消息——本项目独有的设计。
4. **事件抢占状态**。Generative Agents 有 re-planning 但没有"抢占"语义；Concordia 的 GM 有裁决但没有状态抢占。要自己设计，**第一次就记好 R6 日志**，否则抢占逻辑出问题会极难查。
