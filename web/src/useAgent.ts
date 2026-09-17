import { computed, onUnmounted, ref, shallowRef } from 'vue'
import {
  fetchHealth,
  fetchSnapshot,
  subscribeStream,
  type Health,
  type Snapshot,
} from './api'

export type ConnState = 'connecting' | 'live' | 'reconnecting' | 'down'

/**
 * 持有与 agent 的实时连接。
 *
 * 单一数据源：SSE 推来的快照覆盖式更新，不自己拼接状态，
 * 避免前端与后端状态不一致（前端只做展示，不做推算）。
 */
export function useAgent(agentId: string) {
  const snapshot = shallowRef<Snapshot | null>(null)
  const health = ref<Health | null>(null)
  const conn = ref<ConnState>('connecting')
  const error = ref<string | null>(null)

  let unsubscribe: (() => void) | null = null

  const tick = computed(() => snapshot.value?.tick ?? 0)

  /** tick 转游戏时刻（与 Go 侧 clock.go 同一约定：1 tick = 1 分钟）。 */
  const clock = computed(() => {
    const t = tick.value
    if (!t) return '--:--'
    const minutes = ((t % 1440) + 1440) % 1440
    const h = Math.floor(minutes / 60)
    const m = minutes % 60
    return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`
  })

  /** 第几天（从 1 起，便于读"第 3 天 14:20"）。 */
  const day = computed(() => Math.floor(tick.value / 1440) + 1)

  const stream = computed(() => snapshot.value?.stream ?? [])

  async function refresh() {
    try {
      snapshot.value = await fetchSnapshot(agentId)
      error.value = null
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    }
    try {
      health.value = await fetchHealth()
    } catch {
      // 健康检查失败不覆盖主错误：SSE 的状态更权威。
    }
  }

  function start() {
    void refresh()
    unsubscribe = subscribeStream(agentId, {
      onSnapshot(s) {
        snapshot.value = s
        conn.value = 'live'
        error.value = null
      },
      onOpen() {
        conn.value = 'live'
        error.value = null
      },
      onError(message) {
        conn.value = 'reconnecting'
        error.value = message
      },
    })
  }

  onUnmounted(() => unsubscribe?.())

  return {
    snapshot,
    health,
    conn,
    error,
    stream,
    tick,
    clock,
    day,
    refresh,
    start,
  }
}
