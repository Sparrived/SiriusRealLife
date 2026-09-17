<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { stateMeta, type StreamEntry } from '../api'

const props = defineProps<{ entries: StreamEntry[]; follow: boolean }>()

const listEl = ref<HTMLElement | null>(null)
const atBottom = ref(true)

/*
 * 意识流是最有信息量的面板，因此默认跟随最新。
 * 但用户往上翻时必须停住，否则内容会在眼皮底下滚走。
 */
const shown = computed(() => props.entries)

watch(
  () => props.entries.length,
  async () => {
    if (!props.follow || !atBottom.value) return
    await nextTick()
    const el = listEl.value
    if (el) el.scrollTop = el.scrollHeight
  },
)

function onScroll() {
  const el = listEl.value
  if (!el) return
  // 8px 容差：贴底判定不该因为亚像素而抖动。
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 8
}

/*
 * 相邻同一状态的多条合成一组：一次"看 QQ"会写好几条，
 * 逐条挂状态名会非常吵。分组让状态切换成为视觉断点。
 */
const groups = computed(() => {
  const out: { state: string; label: string; items: StreamEntry[] }[] = []
  for (const e of shown.value) {
    const last = out[out.length - 1]
    if (last && last.state === e.state) {
      last.items.push(e)
    } else {
      out.push({ state: e.state, label: stateMeta(e.state).label, items: [e] })
    }
  }
  return out
})
</script>

<template>
  <section class="stream" aria-label="意识流">
    <div v-if="!entries.length" class="empty">
      <p>还没有意识流。</p>
      <p class="hint">agent 每进入一个状态会写下当前在想什么。</p>
    </div>

    <div v-else ref="listEl" class="list" @scroll="onScroll">
      <div v-for="(g, gi) in groups" :key="gi" class="group">
        <div class="ghead">
          <span class="gstate">{{ g.label }}</span>
          <span class="num gtick">t{{ g.items[0]?.seq }}</span>
        </div>
        <p v-for="e in g.items" :key="e.seq + e.text" class="line">{{ e.text }}</p>
      </div>
    </div>

    <p v-if="!follow" class="paused">已暂停跟随，滚动到底部继续</p>
  </section>
</template>

<style scoped>
.stream {
  display: flex;
  flex-direction: column;
  min-height: 0;
  height: 100%;
}

.list {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding-right: 4px;
  scrollbar-width: thin;
}

.group + .group {
  margin-top: 10px;
}

.ghead {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  border-bottom: 1px solid var(--line);
  padding-bottom: 3px;
  margin-bottom: 5px;
}

.gstate {
  font-size: 11px;
  color: var(--text-dim);
}

.gtick {
  font-size: 10px;
  color: var(--text-faint);
}

.line {
  margin: 0 0 3px;
  font-size: 12px;
  color: var(--text);
  /* 长文本换行缩进，让"一条"的边界看得出来 */
  padding-left: 9px;
  text-indent: -9px;
  word-break: break-word;
}

.empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 4px;
}

.empty p {
  margin: 0;
  font-size: 12px;
  color: var(--text-dim);
}

.hint {
  color: var(--text-faint) !important;
  font-size: 11px !important;
}

.paused {
  margin: 6px 0 0;
  font-size: 11px;
  color: var(--warn);
  border-top: 1px solid var(--line);
  padding-top: 5px;
}
</style>
