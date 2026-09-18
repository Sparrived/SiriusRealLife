package fsm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// hasTool 报告工具清单里有没有某个工具。
func hasTool(specs []ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// streamJoined 把意识流拼成一段文本，便于断言"某句话出现了没有"。
func streamJoined(a *Agent) string {
	var b strings.Builder
	for _, e := range a.Stream {
		b.WriteString(e.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// readAppChatter 模拟"信息不够，先看一眼再决定"。
//
// 判据取自 **prompt 文本**（toolReadHint）而不是 agent 字段：Chat 跑在
// 另一个 goroutine 上，读 agent 状态就是跨 goroutine 读写（R1）。
type readAppChatter struct {
	// maxReads 是最多调几次 read_app，用它测预算与有界性。
	maxReads int
	// app 是 read_app 要看的 app，默认 qq（用它测可见性门控）。
	app string
	// readCalls 记录模型**提出**要读几次。
	readCalls int
	// turns 记录每次被调用属于哪一轮。
	turns []thinkKind
}

func (c *readAppChatter) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	// 工具结果轮：prompt 里有 toolReadHint，且**不带任何工具**。
	if strings.Contains(req.Prompt, toolReadHint) {
		c.turns = append(c.turns, thinkToolRead)
		return ChatResponse{Text: "看明白了，回头再回"}, nil
	}
	c.turns = append(c.turns, thinkDecision)
	if c.readCalls < c.maxReads && hasTool(req.Tools, toolReadApp) {
		c.readCalls++
		app := c.app
		if app == "" {
			app = "qq"
		}
		args, _ := json.Marshal(map[string]any{"app": app, "n": 5})
		return ChatResponse{
			Text:      "先翻翻看",
			ToolCalls: []ToolCall{{ID: "r1", Name: toolReadApp, Arguments: args}},
		}, nil
	}
	return ChatResponse{
		Text: "那就这样",
		ToolCalls: []ToolCall{{
			ID: "c1", Name: toolStay,
			Arguments: json.RawMessage(`{"why":"看完了，先这样","for_ticks":5}`),
		}},
	}, nil
}

// countKind 数某种轮次出现了几次。
func countKind(turns []thinkKind, k thinkKind) int {
	n := 0
	for _, x := range turns {
		if x == k {
			n++
		}
	}
	return n
}

// newReadAppAgent 造一个"看着 QQ、能往上翻"的 agent。
func newReadAppAgent(t *testing.T, chatter Chatter, backlog []string, dredge func([]string, Tick) []string) (*Agent, *fakeAttention) {
	t.Helper()
	att := &fakeAttention{backlog: backlog}
	a := newTestAgentWith(t, Options{
		Name: "readapp", States: MVPStates(), Initial: "scrolling_phone",
		Chatter:   chatter,
		Attention: att,
		Dredge:    dredge,
	})
	return a, att
}

// TestReadAppDefersUntilContentIsSeen 验证"先看一眼再决定"的整条流程。
//
// 模型调 read_app 说的是"我信息不够"，因此那一刻**不该**产生状态决定。
// 框架要做到的顺序是：读内容（进意识流 + 待选区）→ 用**刚读到的内容**
// 打捞 → 发起工具结果轮（只读懂与回应）→ 重新问一次状态决策。
func TestReadAppDefersUntilContentIsSeen(t *testing.T) {
	chatter := &readAppChatter{maxReads: 1}
	var queries [][]string
	a, att := newReadAppAgent(t, chatter,
		[]string{"张三: 周末去看展吗", "李四: 听说那个展不错"},
		func(q []string, _ Tick) []string {
			queries = append(queries, q)
			return []string{"上次也是张三约的展"}
		})
	ctx := context.Background()

	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	if att.browsed == 0 {
		t.Fatal("read_app 没有真的翻到内容")
	}
	// 轮次顺序：决策（调 read_app）→ 工具结果轮 → 决策（真的做了决定）。
	want := []thinkKind{thinkDecision, thinkToolRead, thinkDecision}
	if len(chatter.turns) < len(want) {
		t.Fatalf("轮次 = %v, 期望至少 %v", chatter.turns, want)
	}
	for i, w := range want {
		if chatter.turns[i] != w {
			t.Errorf("第 %d 轮 = %s, 期望 %s（全部轮次 %v）", i, chatter.turns[i], w, chatter.turns)
		}
	}

	// 读到的内容真的进了意识流（【刚发生】就是从它装配的）。
	if got := streamJoined(a); !strings.Contains(got, "周末去看展吗") {
		t.Errorf("读到的内容没进意识流:\n%s", got)
	}

	// 打捞只在工具结果轮发生，且查询词是刚读到的内容。
	if len(queries) != 1 {
		t.Fatalf("打捞次数 = %d, 期望 1（只有工具结果轮打捞）", len(queries))
	}
	joined := strings.Join(queries[0], "|")
	if !strings.Contains(joined, "周末去看展吗") {
		t.Errorf("打捞查询词应含刚读到的内容，实际 %v", queries[0])
	}

	// 决策最终落地，且"读"留下的痕迹已清干净。
	if a.LastRecord().Reason == "" {
		t.Fatal("读完内容后应当重新问一次并做出决定")
	}
	if len(a.pendingRead) != 0 || a.readApps != 0 {
		t.Errorf("决策落地后应清空读的痕迹：pendingRead=%v readApps=%d",
			a.pendingRead, a.readApps)
	}
}

// TestToolReadRoundCarriesDredgedMemory 验证工具结果轮真的拿到了记忆。
//
// 这是本次改动的**存在性测试**：打捞只在这一轮发生，所以如果它没进
// prompt，"回复消息需要记忆"就只是一句空话。
func TestToolReadRoundCarriesDredgedMemory(t *testing.T) {
	var prompts []string
	chatter := &promptRecordingChatter{inner: &readAppChatter{maxReads: 1}, prompts: &prompts}
	a, _ := newReadAppAgent(t, chatter,
		[]string{"张三: 周末去看展吗"},
		func([]string, Tick) []string { return []string{"上次也是张三约的展"} })
	ctx := context.Background()

	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	var toolReadPrompt string
	for _, p := range prompts {
		if strings.Contains(p, toolReadHint) {
			toolReadPrompt = p
		}
	}
	if toolReadPrompt == "" {
		t.Fatalf("没有发生工具结果轮，prompts=%d 条", len(prompts))
	}
	if !strings.Contains(toolReadPrompt, "上次也是张三约的展") {
		t.Errorf("工具结果轮里没有打捞回来的记忆：\n%s", toolReadPrompt)
	}
	if !strings.Contains(toolReadPrompt, "周末去看展吗") {
		t.Errorf("工具结果轮里没有刚读到的内容：\n%s", toolReadPrompt)
	}
}

// promptRecordingChatter 记录每次调用的 prompt，其余转发给 inner。
type promptRecordingChatter struct {
	inner   Chatter
	prompts *[]string
}

func (c *promptRecordingChatter) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	*c.prompts = append(*c.prompts, req.Prompt)
	return c.inner.Chat(ctx, req)
}

// toolReadTurnsCarryNoStateTools 验证工具结果轮不带状态工具。
//
// 用 prompt 文本判轮次（R1），再结合"那一轮 chat 请求的 Tools 为空"
// 来断言——由 recordingChatter 一并记录。
func TestToolReadRoundHasNoStateTools(t *testing.T) {
	rec := &toolRecordingChatter{inner: &readAppChatter{maxReads: 1}}
	a, _ := newReadAppAgent(t, rec, []string{"张三: 周末去看展吗"}, nil)
	ctx := context.Background()
	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	sawToolRead := false
	for _, r := range rec.reqs {
		isToolRead := strings.Contains(r.prompt, toolReadHint)
		if !isToolRead {
			continue
		}
		sawToolRead = true
		if len(r.tools) != 0 {
			t.Errorf("工具结果轮不该带工具（它不负责改状态），实际 %v", toolNames(r.tools))
		}
	}
	if !sawToolRead {
		t.Fatal("没有观察到工具结果轮")
	}
	// 反过来：状态决策轮必须仍然带 stay/enter_state。
	sawDecisionTools := false
	for _, r := range rec.reqs {
		if strings.Contains(r.prompt, toolReadHint) {
			continue
		}
		if hasTool(r.tools, toolStay) && hasTool(r.tools, toolEnterState) {
			sawDecisionTools = true
		}
	}
	if !sawDecisionTools {
		t.Error("状态决策轮应当仍然带 stay/enter_state")
	}
}

// toolRecordingChatter 记录每次调用的 prompt 与工具清单。
type toolRecordingChatter struct {
	inner Chatter
	reqs  []struct {
		prompt string
		tools  []ToolSpec
	}
}

func (c *toolRecordingChatter) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	c.reqs = append(c.reqs, struct {
		prompt string
		tools  []ToolSpec
	}{req.Prompt, req.Tools})
	return c.inner.Chat(ctx, req)
}

func toolNames(specs []ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// TestReadAppOfferedOnlyWhenSomethingIsVisible 验证工具可见性随状态变化。
//
// read_app 是"翻开手机看内容"，看不见信息的场合把它摆出来只会诱使
// 模型点一个空动作（R7 修订：被门控的是信息，不是工具本身）。
func TestReadAppOfferedOnlyWhenSomethingIsVisible(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})

	a.Current = "scrolling_phone"
	if !hasTool(a.decisionTools(), toolReadApp) {
		t.Error("看着 QQ 时应当可以「先翻翻看」")
	}

	a.Current = "sleeping"
	if hasTool(a.decisionTools(), toolReadApp) {
		t.Error("睡觉不订阅任何通道，不该提供 read_app——翻开也没内容")
	}
}

// TestReadAppIsBounded 验证"再翻一点"不会变成无限次调用。
//
// 每读一次要多花两次 LLM 调用（工具结果轮 + 重问决策），所以必须有预算。
// 超预算后 read_app 被忽略，模型只能从剩下的选择里挑，或降级为 stay。
func TestReadAppIsBounded(t *testing.T) {
	chatter := &readAppChatter{maxReads: 99} // 模型一直想翻
	a, _ := newReadAppAgent(t, chatter,
		[]string{"张三: 一", "李四: 二", "王五: 三"}, nil)
	ctx := context.Background()

	for i := 0; i < 40 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	if got := countKind(chatter.turns, thinkToolRead); got > maxReadApps {
		t.Errorf("工具结果轮发生了 %d 次，超过预算 %d", got, maxReadApps)
	}
	// 关键：超预算后仍必须做出决定，不能把这次询问丢掉（人格会卡住）。
	if a.LastRecord().Reason == "" {
		t.Fatal("超预算后仍必须做出决定（降级为 stay），不能卡住")
	}
	if a.readApps != 0 {
		t.Errorf("决策落地后预算应清零，实际 %d", a.readApps)
	}
}

// TestReadAppOnInvisibleAppIsIgnored 验证可见性门控。
//
// 看不见的 app 读不到东西，而且这次询问**不能**被丢掉——否则模型
// 挑了一个看不见的 app 之后，人格就卡在原地再也等不到下一次询问。
func TestReadAppOnInvisibleAppIsIgnored(t *testing.T) {
	chatter := &readAppChatter{maxReads: 99, app: "weibo"}
	a, att := newReadAppAgent(t, chatter,
		[]string{"张三: 这条不该被读出来"}, nil)
	ctx := context.Background()

	for i := 0; i < 20 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	if att.browsed != 0 {
		t.Errorf("看不见的 app 不该被读到，实际翻了 %d 条", att.browsed)
	}
	if got := streamJoined(a); strings.Contains(got, "这条不该被读出来") {
		t.Errorf("看不见的 app 的内容泄漏进了意识流:\n%s", got)
	}
	if countKind(chatter.turns, thinkToolRead) != 0 {
		t.Error("读不成时不该发起工具结果轮")
	}
	if a.LastRecord().Reason == "" {
		t.Fatal("读不成也必须做出决定，不能把这次询问丢掉")
	}
}
