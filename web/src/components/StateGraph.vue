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

// candidates 把候选清单摆成"能进的"与"被挡掉的"两组。
//
// 这里**不算权重、不重算抽取**：R3 之后没有随机，谁被选中只看 `to`。
// 真正有排障价值的是 blocked ——"为什么她从来不去睡觉？"是问得最多的问题，
// 而答案（Guard 没过 / 冷却中 / 就是当前状态）只有后端知道。
const blockedMap = computed(() => {
  const m = new Map<string, string>()
  for (const c of dispatch.value?.candidates ?? []) {
    if (c.blocked) m.set(c.name, c.blocked)
  }
  return m
})

const choosable = computed(() =>
  (dispatch.value?.candidates ?? [])
    .filter((c) => !c.blocked)
    .map((c) => ({
      name: c.name,
      label: stateMeta(c.name).label,
      chosen: c.name === dispatch.value?.to,
    })),
)

const blocked = computed(() =>
  (dispatch.value?.candidates ?? [])
    .filter((c) => c.blocked)
    .map((c) => ({
      name: c.name,
      label: stateMeta(c.name).label,
      why: blockedMap.value.get(c.name) ?? '',
    })),
)

/** 决定来源的中文说明（对应 R6 日志的 reason 字段）。 */
const reasonText = computed(() => {
  switch (dispatch.value?.reason) {
    case 'llm':
      return '模型自己选的'
    case 'stay':
      return '留在原状态（模型选的，或调用失败后的降级）'
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
        <span class="dtitle">上次决定</span>
        <span class="num dim">{{ dispatch.from }} → {{ dispatch.to }}</span>
      </div>
      <p class="reason">{{ reasonText }}</p>

      <!-- R6：模型给的理由是"为什么是这个状态"的唯一依据。 -->
      <p v-if="dispatch.why" class="why">「{{ dispatch.why }}」</p>
      <p class="for num">下次再问：{{ dispatch.for_ticks }} tick 后</p>

      <ul v-if="choosable.length" class="cands">
        <li v-for="c in choosable" :key="c.name" :class="{ chosen: c.chosen }">
          <span class="mark" aria-hidden="true">{{ c.chosen ? '▸' : '·' }}</span>
          <span class="cname">{{ c.label }}</span>
        </li>
      </ul>

      <template v-if="blocked.length">
        <p class="sub">当时不能选</p>
        <ul class="cands blocked">
          <li v-for="c in blocked" :key="c.name">
            <span class="mark" aria-hidden="true">×</span>
            <span class="cname">{{ c.label }}</span>
            <span class="cwhy">{{ c.why }}</span>
          </li>
        </ul>
      </template>
    </div>
    <p v-else class="pending">尚未发生状态决定</p>
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

/* 模型给的理由：R6 里最有价值的一行，用正文色而不是弱化色。 */
.why {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--text);
}

.for {
  margin: 0;
  font-size: 11px;
  color: var(--text-faint);
}

.sub {
  margin: 2px 0 0;
  font-size: 10px;
  color: var(--text-faint);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}

.cands {
  display: grid;
  gap: 4px;
  margin: 0;
  padding: 0;
  list-style: none;
}

.cands li {
  display: grid;
  grid-template-columns: 12px auto 1fr;
  align-items: baseline;
  gap: 7px;
}

.mark {
  font-size: 10px;
  color: var(--text-faint);
}

.cname {
  font-size: 11px;
  color: var(--text-faint);
}

.cands li.chosen .mark,
.cands li.chosen .cname {
  color: var(--accent);
}

/* 被挡掉的原因：这一栏回答的是"为什么她不去睡觉"这类最常问的问题。 */
.cands.blocked .cname {
  color: var(--text-dim);
}

.cwhy {
  font-size: 10px;
  line-height: 1.4;
  color: var(--text-faint);
}

.pending {
  margin: 0;
  font-size: 11px;
  color: var(--text-faint);
}
</style>
