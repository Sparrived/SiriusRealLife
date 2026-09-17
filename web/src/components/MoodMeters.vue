<script setup lang="ts">
import { computed } from 'vue'
import type { Mood } from '../api'

const props = defineProps<{ mood: Mood | null }>()

interface Meter {
  key: keyof Mood
  label: string
  note: string
  color: string
}

// 三个连续量（R2：心境与状态正交，不合并）。
// 说明里写的衰减量级来自 fsm.DefaultMoodRates()。
const METERS: Meter[] = [
  { key: 'energy', label: '精力', note: '约一天掉完，睡觉恢复', color: 'var(--accent)' },
  { key: 'annoyed', label: '烦躁', note: '约 4 小时消气', color: 'var(--warn)' },
  { key: 'curious', label: '好奇', note: '约一天半消退', color: 'var(--ok)' },
]

const rows = computed(() =>
  METERS.map((m) => {
    const raw = props.mood ? props.mood[m.key] : 0
    return { ...m, value: Math.max(0, Math.min(100, raw)) }
  }),
)
</script>

<template>
  <section class="mood" aria-label="心境">
    <div v-for="m in rows" :key="m.key" class="row">
      <div class="head">
        <span class="label">{{ m.label }}</span>
        <span class="num value">{{ m.value.toFixed(1) }}</span>
      </div>
      <!-- 轨道只是参照刻度，填充条才承载数值 -->
      <div
        class="track"
        role="meter"
        :aria-label="m.label"
        :aria-valuenow="Math.round(m.value)"
        aria-valuemin="0"
        aria-valuemax="100"
      >
        <div class="fill" :style="{ width: m.value + '%', background: m.color }" />
      </div>
      <p class="note">{{ m.note }}</p>
    </div>
  </section>
</template>

<style scoped>
.mood {
  display: grid;
  gap: 14px;
}

.head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.label {
  color: var(--text-dim);
  font-size: 12px;
}

.value {
  font-size: 13px;
  color: var(--text);
}

.track {
  position: relative;
  height: 3px;
  margin-top: 6px;
  background: var(--bg-inset);
  border-radius: 999px;
  overflow: hidden;
}

.fill {
  height: 100%;
  border-radius: 999px;
  /* 连续量平滑过渡；状态量不做过渡（见状态图）。 */
  transition: width 240ms linear;
}

.note {
  margin: 5px 0 0;
  font-size: 11px;
  color: var(--text-faint);
}
</style>
