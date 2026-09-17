<script setup lang="ts">
import { computed } from 'vue'
import { stateMeta, type Dispatch, type Snapshot } from '../api'

const props = defineProps<{
  snapshot: Snapshot | null
}>()

/** 状态表的固定顺序，与 Go 侧 fsm.MVPStates() 一致。 */
const ORDER = ['scrolling_phone', 'idle', 'working', 'sleeping']

const current = computed(() => props.snapshot?.state ?? '')

const nodes = computed(() =>
  ORDER.map((name) => {
    const meta = stateMeta(name)
    return {
      name,
      ...meta,
      active: name === current.value,
    }
  }),
)

const dispatch = computed<Dispatch | null>(() => props.snapshot?.last_dispatch ?? null)

/** 候选权重按最大值归一，用于条形长度。 */
const candidates = computed(() => {
  const d = dispatch.value
  if (!d?.candidates?.length) return []
  const max = Math.max(...d.candidates.map((c) => c.weight))
  return d.candidates.map((c) => ({
    name: c.name,
    label: stateMeta(c.name).label,
    weight: c.weight,
    pct: max > 0 ? (c.weight / max) * 100 : 0,
    // 哪个候选中标：直接用 to，不在这里重算加权抽取。
    // 重算等于把 Go 侧 dispatch() 的累减逻辑抄一遍，一旦那边调整
    // 就会悄悄不一致；to 本来就是那次抽取的结果。
    chosen: c.name === d.to,
  }))
})

/** 转移原因的中文说明（对应 R6 日志的 reason 字段）。 */
const reasonText = computed(() => {
  switch (dispatch.value?.reason) {
    case 'timeout':
      return '停留到期，加权抽取'
    case 'init':
      return '初始状态'
    default:
      return dispatch.value?.reason ?? ''
  }
})
</script>

<template>
  <section class="graph" aria-label="状态机">
    <ul class="nodes">
      <li
        v-for="n in nodes"
        :key="n.name"
        class="node"
        :class="{ active: n.active }"
        :style="{ '--node-accent': n.accent }"
      >
        <span class="dot" aria-hidden="true" />
        <span class="name">{{ n.label }}</span>
        <span class="key num">{{ n.name }}</span>
        <span v-if="n.seesQQ" class="tag">订阅 QQ</span>
      </li>
    </ul>

    <div v-if="dispatch" class="dispatch">
      <div class="dhead">
        <span class="dtitle">上次分派</span>
        <span class="num dim">{{ dispatch.from }} → {{ dispatch.to }}</span>
      </div>
      <p class="reason">{{ reasonText }}</p>

      <ul class="cands">
        <li v-for="c in candidates" :key="c.name" :class="{ chosen: c.chosen }">
          <span class="cname">{{ c.label }}</span>
          <span class="cbar" aria-hidden="true">
            <i :style="{ width: c.pct + '%' }" />
          </span>
          <span class="num cw">{{ c.weight.toFixed(1) }}</span>
        </li>
      </ul>

      <!-- 把随机数摆出来：这是"为什么是这个状态"的唯一依据（R6）。 -->
      <p class="roll num">
        roll {{ dispatch.roll.toFixed(2) }} / total {{ dispatch.total.toFixed(2) }}
      </p>
    </div>
    <p v-else class="pending">尚未发生状态转移</p>
  </section>
</template>

<style scoped>
.graph {
  display: grid;
  gap: 14px;
}

.nodes {
  display: grid;
  gap: 1px;
  margin: 0;
  padding: 0;
  list-style: none;
  background: var(--line);
  border: 1px solid var(--line);
  border-radius: var(--radius-lg);
  overflow: hidden;
}

/* 用分栏与发丝线分组，不用卡片堆叠（design-taste §4.4）。 */
.node {
  display: grid;
  grid-template-columns: 10px 1fr auto;
  grid-template-areas:
    'dot name tag'
    'dot key  tag';
  align-items: center;
  gap: 0 8px;
  padding: 8px 10px;
  background: var(--bg-raised);
}

.node.active {
  background: color-mix(in srgb, var(--node-accent) 10%, var(--bg-raised));
}

.dot {
  grid-area: dot;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--line-strong);
}

.node.active .dot {
  background: var(--node-accent);
  /* 唯一允许的循环动效：表示"就是它现在活着"。 */
  animation: pulse 2s ease-in-out infinite;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 1;
    box-shadow: 0 0 0 0 color-mix(in srgb, var(--node-accent) 60%, transparent);
  }
  50% {
    opacity: 0.75;
    box-shadow: 0 0 0 4px transparent;
  }
}

.name {
  grid-area: name;
  font-size: 13px;
  color: var(--text-dim);
}

.node.active .name {
  color: var(--text);
}

.key {
  grid-area: key;
  font-size: 10px;
  color: var(--text-faint);
}

.tag {
  grid-area: tag;
  font-size: 10px;
  color: var(--text-faint);
  border: 1px solid var(--line-strong);
  border-radius: 999px;
  padding: 1px 6px;
  white-space: nowrap;
}

.dispatch {
  border-top: 1px solid var(--line);
  padding-top: 12px;
  display: grid;
  gap: 8px;
}

.dhead {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.dtitle {
  font-size: 12px;
  color: var(--text-dim);
}

.dim {
  font-size: 11px;
  color: var(--text-faint);
}

.reason {
  margin: 0;
  font-size: 11px;
  color: var(--text-dim);
}

.cands {
  display: grid;
  gap: 5px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.cands li {
  display: grid;
  grid-template-columns: 52px 1fr 38px;
  align-items: center;
  gap: 8px;
}

.cname {
  font-size: 11px;
  color: var(--text-faint);
}

.cands li.chosen .cname {
  color: var(--text);
}

.cbar {
  height: 3px;
  background: var(--bg-inset);
  border-radius: 999px;
  overflow: hidden;
}

.cbar i {
  display: block;
  height: 100%;
  background: var(--muted);
  border-radius: 999px;
  transition: width 200ms ease-out;
}

.cands li.chosen .cbar i {
  background: var(--accent);
}

.cw {
  font-size: 11px;
  color: var(--text-faint);
  text-align: right;
}

.cands li.chosen .cw {
  color: var(--text);
}

.roll {
  margin: 0;
  font-size: 11px;
  color: var(--text-faint);
}

.pending {
  margin: 0;
  font-size: 11px;
  color: var(--text-faint);
}
</style>
