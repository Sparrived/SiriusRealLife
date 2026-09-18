# web：Sirius 观测台前端

Vue 3 + Vite + TypeScript。**只读展示 + 投递消息**，不含任何业务逻辑：
状态机、心境、记忆全在后端，前端不推算、不缓存真相。

## 开发

```bash
npm install
npm run dev        # http://127.0.0.1:5173，/api 与 /amkr 自动代理到 :8080
```

先跑起后端：

```bash
go run ./cmd/sirius    # 默认 127.0.0.1:8080
```

## 构建

```bash
npm run build      # 对比度检查 → 类型检查 → 打包到 web/dist
```

`SIRIUS_STATIC_DIR=web/dist` 时由 Go 侧当静态目录提供（Dockerfile 里已设）。

## 结构

```
src/
  api.ts                         API 类型 + SSE 订阅（字段与 Go 侧 snake_case 对齐）
  useAgent.ts                    实时连接状态（SSE 是唯一数据源）
  App.vue                        三栏布局
  styles/base.css                设计令牌（颜色、字号、圆角）
  components/
    StateGraph.vue               状态机 + 上次决定（候选 / 被挡掉的原因 / why）
    MoodMeters.vue               三个连续心境量
    MemoryPanel.vue              记忆各层条数（待选区为 0 会告警）
    ConsciousnessStream.vue      意识流（按状态分组，带类型标记）
check-contrast.mjs               WCAG AA 断言，读真实 CSS 变量
```

## 约定

- **SSE 而非 WebSocket**：只用 `EventSource`（`docs/conventions.md` §3）。
- **字段 snake_case**：与 `internal/transport/server.go` 的 `stateResponse` 一一对应。
  **改后端响应形状必须同步改 `src/api.ts`** —— 这条不是客套：R3 取消随机后
  后端去掉了 `roll`/`total` 与候选的 `weight`，前端没跟上，于是
  `StateGraph.vue` 里的 `dispatch.roll.toFixed(2)` 拿到 `undefined` 直接抛
  TypeError，状态机那一栏从那天起就渲染不出来，而且没有任何测试会发现
  （前端只有构建检查，没有契约测试）。
- **不做乐观更新**：投递消息后不改本地状态，等后端推回来。
- **意识流的类型标记不只用颜色**：`·` 看到 / `~` 想 / `›` 做 / `→` 打算。
  符号本身就能区分，颜色只是辅助，色盲用户同样可读。
  兜底 `kindMeta()` 保证后端将来加类型时界面不崩。
- **颜色改动要通过 `npm run check:contrast`**：它读 `base.css` 的真实变量值，
  断言每级文字在不低于 4.5:1 的对比度上（本项目文字多为 10–13px，按小字算）。
- 动效一律包在 `prefers-reduced-motion` 里；本页只有一处循环动效
  （当前状态的点在呼吸），其余都是状态变化的反馈。

## 设计取向

这是一个**实时观测仪表盘**，不是营销页，因此套用
`design-taste-frontend` 时用它的反面清单而不是落地页套路：
`DESIGN_VARIANCE 6 / MOTION_INTENSITY 4 / VISUAL_DENSITY 6`。

刻意避开：AI 紫渐变、玻璃拟态、三等分卡片、Inter 默认字体（用系统栈，
本地工具不引 Google Fonts）、装饰性 SVG、emoji、`h-screen`（用 `100dvh`）、
em-dash。

配色是近黑冷调基底 + **单一**强调色（青绿，代表"活跃"）。
状态色（`--ok` / `--sleep` / `--muted`）是**语义例外**：四个状态需要可区分，
这是一份写在 `base.css` 里的系统，不是随手加的颜色。
圆角统一一套（4px / 6px）。
