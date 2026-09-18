<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import ConsciousnessStream from './components/ConsciousnessStream.vue'
import MemoryPanel from './components/MemoryPanel.vue'
import MoodMeters from './components/MoodMeters.vue'
import StateGraph from './components/StateGraph.vue'
import { postEvent, stateMeta } from './api'
import { useAgent } from './useAgent'

// agent id 固定为 sirius（与 cmd/sirius 的 AgentID 一致）。
const AGENT = 'sirius'
const a = useAgent(AGENT)
onMounted(() => a.start())

const currentMeta = computed(() => stateMeta(a.snapshot.value?.state ?? ''))

/** 手动投递消息的输入。 */
const draft = ref('')
const mentions = ref(false)
const sending = ref(false)
const sendError = ref<string | null>(null)
const sent = ref(0)

async function send() {
  const text = draft.value.trim()
  if (!text || sending.value) return
  sending.value = true
  sendError.value = null
  try {
    await postEvent(AGENT, text, mentions.value)
    draft.value = ''
    sent.value++
  } catch (e) {
    sendError.value = e instanceof Error ? e.message : String(e)
  } finally {
    sending.value = false
  }
}

const connLabel: Record<string, string> = {
  connecting: '连接中',
  live: '实时',
  reconnecting: '重连中',
  down: '已断开',
}

const follow = computed(() => a.conn.value === 'live')
</script>

<template>
  <div class="app">
    <header class="bar">
      <div class="brand">
        <span class="mark" aria-hidden="true" />
        <span class="title">Sirius</span>
        <span class="sub">人格观测台</span>
      </div>

      <div class="vitals">
        <span class="clock num">{{ a.clock.value }}</span>
        <span class="day num">第 {{ a.day.value }} 天</span>
        <span class="sep" aria-hidden="true" />
        <span class="tick num">t{{ a.tick.value }}</span>
        <span class="conn" :data-state="a.conn.value">
          <i aria-hidden="true" />
          {{ connLabel[a.conn.value] }}
        </span>
      </div>
    </header>

    <p v-if="a.error.value" class="banner" role="status">{{ a.error.value }}</p>

    <main class="grid">
      <aside class="col left">
        <section class="panel">
          <h2 class="ph">当前状态</h2>
          <p class="state" :style="{ '--c': currentMeta.accent }">
            <span class="sdot" aria-hidden="true" />
            {{ currentMeta.label }}
          </p>
          <p class="shint">{{ currentMeta.hint }}</p>
        </section>

        <section class="panel">
          <h2 class="ph">心境</h2>
          <MoodMeters :mood="a.snapshot.value?.mood ?? null" />
        </section>

        <section class="panel">
          <h2 class="ph">内在</h2>
          <ul class="stats">
            <li>
              <span>LLM 调用</span>
              <span class="num">{{ a.snapshot.value?.call_count ?? 0 }}</span>
            </li>
            <li>
              <span>思考中</span>
              <span class="num">{{ a.snapshot.value?.thinking ? '是' : '否' }}</span>
            </li>
          </ul>
        </section>

        <section class="panel">
          <h2 class="ph">记忆</h2>
          <MemoryPanel :memory="a.snapshot.value?.memory ?? null" />
        </section>

        <section class="panel">
          <h2 class="ph">AMKR</h2>
          <p class="amkr">
            <span class="adot" :data-ready="a.health.value?.amkr.ready ? '1' : '0'" aria-hidden="true" />
            {{ a.health.value ? (a.health.value.amkr.ready ? '可用' : '不可用') : '未知' }}
          </p>
          <a class="link" href="/amkr/ui/" target="_blank" rel="noopener">打开 AMKR 管理台</a>
        </section>
      </aside>

      <section class="col mid">
        <h2 class="ph">状态机</h2>
        <StateGraph :snapshot="a.snapshot.value" />
      </section>

      <section class="col right">
        <div class="rhead">
          <h2 class="ph">意识流</h2>
          <span class="num rcount">{{ a.stream.value.length }}</span>
        </div>
        <ConsciousnessStream :entries="a.stream.value" :follow="follow" />

        <form class="inject" @submit.prevent="send">
          <label class="ih" for="msg">投递消息</label>
          <div class="irow">
            <input
              id="msg"
              v-model="draft"
              type="text"
              placeholder="说点什么…"
              autocomplete="off"
            />
            <button type="submit" :disabled="sending || !draft.trim()">
              {{ sending ? '发送中' : '发送' }}
            </button>
          </div>
          <label class="check">
            <input v-model="mentions" type="checkbox" />
            <span>@ 她（只是更显眼，不打断当前状态）</span>
          </label>
          <p v-if="sendError" class="ierr" role="alert">{{ sendError }}</p>
          <p v-else-if="sent" class="iok">已投递 {{ sent }} 条</p>
          <p class="inote">
            消息不会立刻进意识流：只有在她进入「刷手机」时才看得到，其余状态静默入队。
          </p>
        </form>
      </section>
    </main>
  </div>
</template>

<style scoped>
.app {
  display: flex;
  flex-direction: column;
  min-height: 100dvh;
  max-width: 1400px;
  margin: 0 auto;
  padding: 0 16px 16px;
}

/* 顶栏单行、高度受控（design-taste §4.7）。 */
.bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  height: 52px;
  border-bottom: 1px solid var(--line);
  flex-wrap: wrap;
}

.brand {
  display: flex;
  align-items: baseline;
  gap: 8px;
}

.mark {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--accent);
  align-self: center;
}

.title {
  font-size: 14px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.sub {
  font-size: 11px;
  color: var(--text-faint);
}

.vitals {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 12px;
  color: var(--text-dim);
}

.clock {
  font-size: 15px;
  color: var(--text);
}

.day,
.tick {
  font-size: 11px;
  color: var(--text-faint);
}

.sep {
  width: 1px;
  height: 12px;
  background: var(--line-strong);
}

.conn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 11px;
  color: var(--text-faint);
}

.conn i {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--muted);
}

.conn[data-state='live'] i {
  background: var(--accent);
}

.conn[data-state='live'] {
  color: var(--accent);
}

.conn[data-state='reconnecting'] i,
.conn[data-state='reconnecting'] {
  color: var(--warn);
}

.conn[data-state='reconnecting'] i {
  background: var(--warn);
}

.conn[data-state='down'] i {
  background: #d4564a;
}

.banner {
  margin: 8px 0 0;
  padding: 6px 10px;
  font-size: 11px;
  color: var(--warn);
  background: var(--bg-raised);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius);
}

/* 三栏：左窄（心境/内在）、中（状态机）、右宽（意识流）。 */
.grid {
  flex: 1;
  min-height: 0;
  display: grid;
  grid-template-columns: 1fr;
  gap: 20px;
  padding-top: 16px;
}

@media (min-width: 900px) {
  .grid {
    grid-template-columns: 210px 260px minmax(0, 1fr);
  }
}

.col {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 18px;
}

/* 右栏占满高度，让意识流自己内部滚动而不是把整页顶长。 */
@media (min-width: 900px) {
  .right {
    max-height: calc(100dvh - 52px - 16px - 1px);
  }
}

.ph {
  margin: 0 0 8px;
  font-size: 11px;
  font-weight: 500;
  color: var(--text-faint);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.state {
  display: flex;
  align-items: center;
  gap: 7px;
  margin: 0;
  font-size: 17px;
  color: var(--text);
}

.sdot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--c);
  flex: none;
}

.shint {
  margin: 5px 0 0;
  font-size: 11px;
  color: var(--text-faint);
}

.stats {
  margin: 0;
  padding: 0;
  list-style: none;
  display: grid;
  gap: 6px;
}

.stats li {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  color: var(--text-dim);
}

.stats .num {
  color: var(--text);
}

.amkr {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 0 0 6px;
  font-size: 12px;
  color: var(--text);
}

.adot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--muted);
}

.adot[data-ready='1'] {
  background: var(--accent);
}

.link {
  font-size: 11px;
  color: var(--text-dim);
  text-decoration: none;
  border-bottom: 1px solid var(--line-strong);
}

.link:hover {
  color: var(--accent);
  border-bottom-color: var(--accent);
}

.rhead {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.rcount {
  font-size: 11px;
  color: var(--text-faint);
}

.inject {
  border-top: 1px solid var(--line);
  padding-top: 10px;
  display: grid;
  gap: 6px;
  flex: none;
}

.ih {
  font-size: 11px;
  color: var(--text-faint);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.irow {
  display: flex;
  gap: 6px;
}

.irow input {
  flex: 1;
  min-width: 0;
  padding: 6px 8px;
  font: inherit;
  font-size: 12px;
  color: var(--text);
  background: var(--bg-inset);
  border: 1px solid var(--line-strong);
  border-radius: var(--radius);
}

.irow input::placeholder {
  color: var(--text-faint);
}

.irow input:focus-visible,
.irow button:focus-visible,
.check input:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}

.irow button {
  padding: 6px 12px;
  font: inherit;
  font-size: 12px;
  color: var(--bg);
  background: var(--accent);
  border: 1px solid var(--accent);
  border-radius: var(--radius);
  cursor: pointer;
  white-space: nowrap;
}

.irow button:hover:not(:disabled) {
  background: color-mix(in srgb, var(--accent) 85%, white);
}

.irow button:active:not(:disabled) {
  transform: translateY(1px);
}

.irow button:disabled {
  color: var(--text-faint);
  background: var(--bg-inset);
  border-color: var(--line-strong);
  cursor: not-allowed;
}

.check {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  color: var(--text-dim);
  cursor: pointer;
}

.check input {
  accent-color: var(--accent);
}

.ierr {
  margin: 0;
  font-size: 11px;
  color: #e0726a;
}

.iok {
  margin: 0;
  font-size: 11px;
  color: var(--accent);
}

.inote {
  margin: 0;
  font-size: 11px;
  color: var(--text-faint);
}
</style>
