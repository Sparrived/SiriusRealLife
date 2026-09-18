// API 类型与 internal/transport/server.go 的 stateResponse 一一对应。
// 字段是 snake_case（conventions §3）。改后端形状时这里必须同步。

export interface Mood {
  energy: number
  annoyed: number
  curious: number
}

// StreamKind 是意识流记录的类型（对应 Go 侧 fsm.Kind）。
//
// 后端加了 Kind 之后前端必须跟上：否则"在想什么/做了什么/打算做什么"
// 在界面上是一团同样的文本，分类的价值就丢了。
export type StreamKind = 'observation' | 'thought' | 'action' | 'intent'

export interface StreamEntry {
  seq: number
  kind: StreamKind
  state: string
  text: string
}

// KIND_META 给每种记录类型一个标记与说明。
//
// 只用符号不只用颜色：色盲用户也要能分辨类型。
export const KIND_META: Record<StreamKind, { mark: string; label: string }> = {
  observation: { mark: '·', label: '看到' },
  thought: { mark: '~', label: '想' },
  action: { mark: '›', label: '做' },
  intent: { mark: '→', label: '打算' },
}

// kindMeta 兜底：后端将来加类型时界面不崩。
export function kindMeta(kind: string): { mark: string; label: string } {
  return KIND_META[kind as StreamKind] ?? { mark: '·', label: kind }
}

// Candidate 是一个候选状态。
//
// **没有权重**：R3 之后框架里没有随机，也不做加权抽取——候选清单只是
// "此刻可以进哪些"，外加不能进的原因。旧的 weight 是加权抽取时代的残留，
// 后端早已不再返回，留在这里会让渲染直接抛 undefined.toFixed。
export interface Candidate {
  name: string
  /** 非空表示它没进候选，值是原因（中文，给人看的）。 */
  blocked?: string
}

export interface Dispatch {
  from: string
  to: string
  /**
   * 决定的来源。只有两个取值：
   *   llm  —— 模型选了另一个状态（这是唯一的"转移"）
   *   stay —— 继续留在原状态（模型主动选的，或调用失败后的降级）
   * 抢占取消后没有 preempt，框架不再有任何能改变状态的来源。
   */
  reason: string
  /** 模型给的理由（第一人称）。这是"为什么是这个状态"的**唯一依据**（R6）。 */
  why: string
  /** 这次决定的停留时长（夹紧之后的实际值）。 */
  for_ticks: number
  candidates: Candidate[]
}

// Memory 是记忆各层条数（memory.md §3）。
//
// staging 长期为 0 就是故障：看手机时看到的内容应当持续写入那里。
// 这条链路出过"四层都有实现、都有单测、但没有任何写入方"的故障，
// 界面上必须能一眼看出它是不是空的。
export interface Memory {
  /** unread 队列条数（含已读，§2.2）。 */
  messages: number
  /** 待选区条数。为 0 即写入链路断了。 */
  staging: number
  events: number
  /** 终态存档条数（LLM 不可读，§3.1）。 */
  shadow: number
}

export interface Snapshot {
  agent_id: string
  tick: number
  state: string
  mood: Mood
  thinking: boolean
  call_count: number
  stream: StreamEntry[] | null
  memory?: Memory
  last_dispatch?: Dispatch
}

export interface Health {
  status: string
  amkr: { ready: boolean }
  subscribers: number
}

// 状态名的显示信息。与 Go 侧 fsm.MVPStates() 对应。
export interface StateMeta {
  label: string
  hint: string
  /** 该状态是否订阅 QQ（对应 State.Channels 含 ChanQQ）。 */
  seesQQ: boolean
  accent: string
}

export const STATE_META: Record<string, StateMeta> = {
  scrolling_phone: {
    label: '刷手机',
    hint: '订阅 QQ，驻留期间消息持续可见',
    seesQQ: true,
    accent: 'var(--accent)',
  },
  idle: {
    label: '发呆',
    hint: '什么都没做',
    seesQQ: false,
    accent: 'var(--muted)',
  },
  working: {
    label: '干活',
    hint: '精力随时间消耗',
    seesQQ: false,
    accent: 'var(--ok)',
  },
  sleeping: {
    label: '睡觉',
    hint: '不订阅 QQ，手机响了也看不见',
    seesQQ: false,
    accent: 'var(--sleep)',
  },
}

export function stateMeta(name: string): StateMeta {
  return (
    STATE_META[name] ?? {
      label: name,
      hint: '未在状态表中登记',
      seesQQ: false,
      accent: 'var(--muted)',
    }
  )
}

const API = '/api/v1'

export async function fetchSnapshot(agentId: string): Promise<Snapshot> {
  const res = await fetch(`${API}/agents/${encodeURIComponent(agentId)}`)
  if (!res.ok) throw new Error(`状态请求失败：HTTP ${res.status}`)
  return (await res.json()) as Snapshot
}

export async function fetchHealth(): Promise<Health> {
  const res = await fetch(`${API}/health`)
  if (!res.ok) throw new Error(`健康检查失败：HTTP ${res.status}`)
  return (await res.json()) as Health
}

export async function postEvent(
  agentId: string,
  text: string,
  mentionsMe: boolean,
): Promise<void> {
  const res = await fetch(`${API}/agents/${encodeURIComponent(agentId)}/events`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text, mentions_me: mentionsMe }),
  })
  if (!res.ok) {
    const detail = await res.text()
    throw new Error(`投递失败：HTTP ${res.status} ${detail}`)
  }
}

export type StreamEventName = 'state' | 'transition'

export interface StreamHandlers {
  onSnapshot: (s: Snapshot, kind: StreamEventName) => void
  onOpen?: () => void
  onError?: (message: string) => void
}

/**
 * 订阅 SSE 实时流（conventions §3：用 SSE，不上 WebSocket）。
 *
 * 返回取消订阅的函数。EventSource 自带重连，但连续失败时会静默重试，
 * 所以这里报告状态给界面，让"掉线"是可见的而不是一片假死。
 */
export function subscribeStream(agentId: string, handlers: StreamHandlers): () => void {
  const url = `${API}/agents/${encodeURIComponent(agentId)}/stream`
  const es = new EventSource(url)

  const handle = (kind: StreamEventName) => (ev: MessageEvent) => {
    try {
      handlers.onSnapshot(JSON.parse(ev.data) as Snapshot, kind)
    } catch {
      handlers.onError?.('收到无法解析的推送')
    }
  }

  const onState = handle('state')
  const onTransition = handle('transition')
  es.addEventListener('state', onState)
  es.addEventListener('transition', onTransition)

  es.onopen = () => handlers.onOpen?.()
  es.onerror = () => {
    // readyState=CONNECTING 表示 EventSource 正在自动重连；
    // CLOSED 表示它放弃了（例如 4xx），这时必须让用户知道。
    if (es.readyState === EventSource.CLOSED) {
      handlers.onError?.('实时连接已断开')
    } else {
      handlers.onError?.('实时连接中断，正在重连…')
    }
  }

  return () => {
    es.removeEventListener('state', onState)
    es.removeEventListener('transition', onTransition)
    es.close()
  }
}
