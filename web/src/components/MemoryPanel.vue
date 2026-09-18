<script setup lang="ts">
import { computed } from 'vue'
import type { Memory } from '../api'

const props = defineProps<{ memory: Memory | null }>()

// 各层的含义（memory.md §3）。写在这里而不是散在模板里：
// 排障时看的正是"这一层该不该有东西"。
const LAYERS: { key: keyof Memory; label: string; hint: string }[] = [
  { key: 'messages', label: 'unread 队列', hint: '收到的消息都留着，按重要性淘汰' },
  { key: 'staging', label: '待选区', hint: '看手机时看到的 + 她说过的话' },
  { key: 'events', label: '事件记忆', hint: '被打捞够了而升格' },
  { key: 'shadow', label: 'Shadow', hint: '衰减到阈值的存档，LLM 读不到' },
]

const rows = computed(() =>
  LAYERS.map((l) => ({ ...l, value: props.memory?.[l.key] ?? 0 })),
)

// staging 为 0 只在"系统确实动过"时才算异常。
//
// 全新启动时它本来就是 0（连消息都还没有），那时报出来是假告警——
// 而假告警的代价是训练人忽略这个面板，正好毁掉它存在的理由。
// 所以要求另外三层里有任何一个非 0：收过消息、升格过、或遗忘过。
const stagingEmpty = computed(() => {
  const m = props.memory
  if (!m) return false
  const activity = (m.messages ?? 0) + (m.events ?? 0) + (m.shadow ?? 0)
  return activity > 0 && (m.staging ?? 0) === 0
})
</script>

<template>
  <div v-if="memory" class="mem">
    <ul class="rows">
      <li v-for="r in rows" :key="r.key" :class="{ alarm: r.key === 'staging' && stagingEmpty }">
        <span class="lbl" :title="r.hint">{{ r.label }}</span>
        <span class="num val">{{ r.value }}</span>
      </li>
    </ul>
    <p v-if="stagingEmpty" class="warn">
      收到过消息或遗忘过，但待选区一条都没有。若她刚看过手机，说明写入链路断了。
    </p>
    <p v-else class="note">待选区：看到的内容与她说过的话都写在这里。</p>
  </div>
  <p v-else class="pending">尚未收到快照</p>
</template>

<style scoped>
.mem {
  display: grid;
  gap: 6px;
}

.rows {
  margin: 0;
  padding: 0;
  list-style: none;
  display: grid;
  gap: 5px;
}

.rows li {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  color: var(--text-dim);
}

.lbl {
  /* 点上去能看到这一层是干什么的，但正文不因此变长。 */
  cursor: help;
  border-bottom: 1px dotted var(--line-strong);
}

.val {
  color: var(--text);
}

/* 唯一的告警态：待选区为 0。用 warn 而不是红，这是"要去看一眼"不是"崩了"。 */
.rows li.alarm .lbl,
.rows li.alarm .val {
  color: var(--warn);
}

.warn {
  margin: 0;
  font-size: 10px;
  line-height: 1.45;
  color: var(--warn);
}

.note,
.pending {
  margin: 0;
  font-size: 10px;
  line-height: 1.45;
  color: var(--text-faint);
}
</style>
