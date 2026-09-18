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

// staging 长期为 0 是**故障信号**，不是"她很干净"。
//
// 这条链路出过一次最难发现的故障：四层记忆都有实现、都有单测，但没有任何
// 写入方——待选区恒为空、打捞恒空、升格与 Shadow 永不发生，而接口看着全对。
// 所以这个面板存在的意义就是让那件事一眼可见。
const stagingEmpty = computed(() => props.memory != null && (props.memory.staging ?? 0) === 0)
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
      待选区为空。若她刚看过手机，说明写入链路断了。
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
