# 编码约定

## 1. Go

- Go 1.26+，模块 `github.com/Sparrived/SiriusRealLife`
- **标准库优先**。HTTP 用 `net/http` + Go 1.22 起的 `ServeMux` 路由模式，不引 gin/echo。JSON 用 `encoding/json`。日志用 `log/slog`。配置用 `encoding/json` 或直接写 Go 表
- 加依赖前先问：标准库能不能做？已有依赖能不能做？两个都不能才加，并在 commit 里说明原因
- 状态表 v1 直接写在 Go 源码里（编译期可查错）。等真的需要不重启调人格时，再迁到 `config/` 并做热加载
- 错误必须 `fmt.Errorf("...: %w", err)` 包装，禁止裸 `return err` 丢上下文
- `context.Context` 作为第一个参数贯穿所有 IO
- 不用 ORM。要持久化就 `database/sql` + 具体驱动

## 2. Vue

- Vue 3 `<script setup>` + TypeScript + Vite
- 状态管理优先 `ref`/`computed`，跨组件才上 Pinia
- 后端实时流用 **SSE**（`EventSource`），不上 WebSocket、不上 Socket.IO
- 组件文件 PascalCase，组合式函数 `useXxx.ts`

## 3. API

- 统一前缀 `/api/v1`
- 请求/响应 JSON，字段 `snake_case`
- 实时流：`GET /api/v1/agents/{id}/stream`（SSE，事件类型标识消息种类）
- 错误响应：`{"error": "...", "detail": "..."}`，HTTP 状态码如实反映

## 4. 提交与协作

- Conventional Commits：`feat:` `fix:` `refactor:` `chore:` `docs:`
- 中文或英文正文都可以，一句话说清"为什么"，diff 已经说明"做了什么"
- 不留注释掉的死代码，不留 `TODO` 而不写跟进条件；要留就写 `ponytail: <上限>，<升级路径>`
- 非平凡逻辑（分支、循环、解析、权限路径）必须留下**一个**能跑的检查：`demo()` 形式的自检或一个小 `_test.go`。不搭测试框架，不写每函数一套 fixture
- 机密（API key、token）永不入库。`.env` 已在 `.gitignore`，只提交 `.env.example`
- **本仓库已公开。** 提交前自查：没有上游 key、没有 AMKR 的 `local_api_key`、没有 `router-config.json`

## 5. 文档

- 本文与 [`architecture.md`](architecture.md)、[`llm-amkr.md`](llm-amkr.md)、[`roadmap.md`](roadmap.md) 是约定的唯一来源；[`../AGENTS.md`](../AGENTS.md) 只放必须时刻生效的规则和索引
- 改约定就改文档，别只改代码
