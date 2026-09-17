// API 类型与 internal/transport/server.go 的 stateResponse 一一对应。
// 字段是 snake_case（conventions §3）。改后端形状时这里必须同步。

export interface Mood {
  energy: number
  annoyed: number
  curious: number
}

export interface StreamEntry {
  seq: number
  state: string
  text: string
}

export interface Candidate {
  name: string
  weight: number
}

export interface Dispatch {
  from: string
  to: string
  reason: string
  roll: number
  total: number
  candidates: Candidate[]
}

export interface Snapshot {
  agent_id: string
  tick: number
  state: string
  mood: Mood
  thinking: boolean
  deferred: number
  call_count: number
  stream: StreamEntry[] | null
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
  /** 该状态是否能看到 QQ（对应 State.Visibility.QQ）。 */
  seesQQ: boolean
  /** 是否不可打断（对应 State.Uninterruptible，如 sleeping）。 */
  uninterruptible: boolean
  accent: string
}

export const STATE_META: Record<string, StateMeta> = {
  scrolling_phone: {
    label: '刷手机',
    hint: '唯一看得到 QQ 的状态',
    seesQQ: true,
    uninterruptible: false,
    accent: 'var(--accent)',
  },
  idle: {
    label: '发呆',
    hint: '什么都没做',
    seesQQ: false,
    uninterruptible: false,
    accent: 'var(--muted)',
  },
  working: {
    label: '干活',
    hint: '精力随时间消耗',
    seesQQ: false,
    uninterruptible: false,
    accent: 'var(--ok)',
  },
  sleeping: {
    label: '睡觉',
    hint: '不可打断，被 @ 只记下延迟处理',
    seesQQ: false,
    uninterruptible: true,
    accent: 'var(--sleep)',
  },
}

export function stateMeta(name: string): StateMeta {
  return (
    STATE_META[name] ?? {
      label: name,
      hint: '未在状态表中登记',
      seesQQ: false,
      uninterruptible: false,
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
