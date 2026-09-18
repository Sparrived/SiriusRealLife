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

	// 决策最终落地，且"刚读到的内容"已清干净（它是**这一次询问**的东西；
	// 读的次数预算按驻留算，不在这里清，由 TestReadAppIsBounded 守）。
	if a.LastRecord().Reason == "" {
		t.Fatal("读完内容后应当重新问一次并做出决定")
	}
	if len(a.pendingRead) != 0 {
		t.Errorf("决策落地后应清空 pendingRead，实际 %v", a.pendingRead)
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

// TestToolReadRoundHasOnlySendMessage 验证工具结果轮的工具面。
//
// 它**不带状态工具**（stay/enter_state）：一次决定只改一次状态，回复不是
// 改状态（R11）。但它**必须带 send_message**——读了消息却回不了话，
// 那一轮就白读了。
func TestToolReadRoundHasOnlySendMessage(t *testing.T) {
	rec := &toolRecordingChatter{inner: &readAppChatter{maxReads: 1}}
	a, _ := newReadAppAgent(t, rec, []string{"张三: 周末去看展吗"}, nil)
	ctx := context.Background()
	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	sawToolRead := false
	for _, r := range rec.reqs {
		if !strings.Contains(r.prompt, toolReadHint) {
			continue
		}
		sawToolRead = true
		if hasTool(r.tools, toolStay) || hasTool(r.tools, toolEnterState) {
			t.Errorf("工具结果轮不该带状态工具（它不改状态），实际 %v", toolNames(r.tools))
		}
		if !hasTool(r.tools, toolSendMessage) {
			t.Errorf("工具结果轮必须带 %s，否则读了消息也回不了话，实际 %v",
				toolSendMessage, toolNames(r.tools))
		}
	}
	if !sawToolRead {
		t.Fatal("没有观察到工具结果轮")
	}

	// 反过来：状态决策轮必须带 stay/enter_state，且**不带** send_message
	// ——回复不是改状态，两个工具面不重叠。
	sawDecisionTools := false
	for _, r := range rec.reqs {
		if strings.Contains(r.prompt, toolReadHint) {
			continue
		}
		if hasTool(r.tools, toolStay) && hasTool(r.tools, toolEnterState) {
			sawDecisionTools = true
			if hasTool(r.tools, toolSendMessage) {
				t.Errorf("状态决策轮不该带 %s，实际 %v", toolSendMessage, toolNames(r.tools))
			}
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
	// 预算按**驻留**算：几次 stay 之后仍然耗尽，不能因为重新决策就回血
	// ——线上实测过按决策算会变成"决定→翻→没捞到→决定→再翻"的死循环
	// （50 tick 烧掉 24 次调用，行为上也像个强迫症）。
	if a.readApps != maxReadApps {
		t.Errorf("预算应被耗尽且不因重新决策而回血，实际 %d（上限 %d）",
			a.readApps, maxReadApps)
	}
	// 拒绝**必须让模型看见**：只写日志的话它会一直重试同一件事。
	// 线上实测：不告诉它，它就每 3 tick 重念一遍"先翻开 QQ"再被拒。
	if got := streamJoined(a); !strings.Contains(got, readAppBudgetHint) {
		t.Errorf("预算用尽时应在意识流里留下提示，实际:\n%s", got)
	}

	// 换状态才重置：新的一次驻留可以重新翻。
	if err := a.commitEnter("working", 5, "换个事做"); err != nil {
		t.Fatalf("commitEnter: %v", err)
	}
	if a.readApps != 0 {
		t.Errorf("换状态后预算应重置，实际 %d", a.readApps)
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

// fakeRecorder 记录"她说过的话"，用于验证 send_message 真的进了记忆。
type fakeRecorder struct{ got []string }

func (f *fakeRecorder) Record(text string) { f.got = append(f.got, text) }

// fakeOutbox 记录被送出去的消息。
type fakeOutbox struct{ got []OutgoingMessage }

func (o *fakeOutbox) Send(m OutgoingMessage) { o.got = append(o.got, m) }

// sendChatter 在工具结果轮里真的回一句，其余同 readAppChatter。
type sendChatter struct {
	read    int
	to      string
	text    string
	prompts []string
}

func (c *sendChatter) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	if strings.Contains(req.Prompt, toolReadHint) {
		c.prompts = append(c.prompts, req.Prompt)
		args, _ := json.Marshal(map[string]any{"to": c.to, "text": c.text})
		return ChatResponse{
			Text: "我跟他说一声",
			ToolCalls: []ToolCall{
				{ID: "s1", Name: toolSendMessage, Arguments: args},
			},
		}, nil
	}
	if !hasTool(req.Tools, toolStay) {
		// 独白轮之类：只回文本。
		return ChatResponse{Text: "随便想想"}, nil
	}
	if c.read < 1 && hasTool(req.Tools, toolReadApp) {
		c.read++
		return ChatResponse{
			Text:      "先翻翻看",
			ToolCalls: []ToolCall{{ID: "r1", Name: toolReadApp, Arguments: json.RawMessage(`{"app":"qq","n":5}`)}},
		}, nil
	}
	return ChatResponse{
		Text: "先这样",
		ToolCalls: []ToolCall{{
			ID: "c1", Name: toolStay,
			Arguments: json.RawMessage(`{"why":"说完了","for_ticks":5}`),
		}},
	}, nil
}

// TestSendMessageEntersStreamMemoryAndOutbox 验证回复的三条去向。
//
// 三件事缺一不可：
//   - 进意识流：她说出口的话是她做过的事，否则下一轮会重复同一句；
//   - 进记忆：**用户明确定过**"说出去的话肯定要进"——"我说过什么"与
//     "我听到什么"一样是情节记忆，缺一半那段对话就只剩对方在自言自语；
//   - 进 outbox：真的送出去（没有通道时留日志，不假装成功）。
func TestSendMessageEntersStreamMemoryAndOutbox(t *testing.T) {
	chatter := &sendChatter{to: "王五", text: "那个展叫「无尽的素描」"}
	rec := &fakeRecorder{}
	out := &fakeOutbox{}
	att := &fakeAttention{backlog: []string{"王五: 上次说的展览叫啥"}}
	a := newTestAgentWith(t, Options{
		Name: "send", States: MVPStates(), Initial: "scrolling_phone",
		Chatter: chatter, Attention: att, Recorder: rec, Outbox: out,
	})
	ctx := context.Background()
	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}

	// 1) 进意识流，且类型是"动作"（她说出去的话是做过的事，不是想法）。
	if got := streamJoined(a); !strings.Contains(got, "那个展叫「无尽的素描」") {
		t.Errorf("回复没进意识流:\n%s", got)
	}
	foundAction := false
	for _, e := range a.Stream {
		if strings.Contains(e.Text, "无尽的素描") {
			if e.Kind != KindAction {
				t.Errorf("回复应以动作类型入意识流，实际 %v：%q", e.Kind, e.Text)
			}
			foundAction = true
		}
	}
	if !foundAction {
		t.Error("意识流里找不到这条回复")
	}

	// 2) 进记忆（用户定的：说出去的话肯定要进）。
	if len(rec.got) != 1 || !strings.Contains(rec.got[0], "无尽的素描") {
		t.Fatalf("回复应写入记忆，实际 %v", rec.got)
	}
	// 记的是"她说过什么"，带上回给谁才有情节。
	if !strings.Contains(rec.got[0], "王五") {
		t.Errorf("记忆里应含回给谁，实际 %q", rec.got[0])
	}

	// 3) 真的送出去，且送的是**原话**（不含"回王五："这类包装）。
	if len(out.got) != 1 {
		t.Fatalf("应送出 1 条，实际 %d", len(out.got))
	}
	if out.got[0].Text != "那个展叫「无尽的素描」" {
		t.Errorf("送出的正文 = %q, 期望原话", out.got[0].Text)
	}
	if out.got[0].To != "王五" {
		t.Errorf("送出的收件人 = %q", out.got[0].To)
	}
	// 带的是 agent 自己的 tick，不是 wall clock（R8）。
	// 不能断言等于 a.Now：发完之后循环还会推进几个 tick 才做决定。
	if t0 := out.got[0].Tick; t0 <= 0 || t0 > a.Now {
		t.Errorf("送出的 tick = %d 不在 (0, %d] 内，疑似用了 wall clock（R8）", t0, a.Now)
	}

	// 发完之后仍然要回到状态决策：回复不改状态。
	if a.LastRecord().Reason == "" {
		t.Fatal("回复之后仍必须做出状态决定")
	}
}

// TestSendMessageWithoutOutboxStillRecords 验证没有出站通道时的行为。
//
// 话仍进意识流与记忆，只是发不出去——不能因为没接通道就把整句话丢掉，
// 那会让"我说过什么"凭空消失。也不能假装成功：sendMessage 会记日志。
func TestSendMessageWithoutOutboxStillRecords(t *testing.T) {
	chatter := &sendChatter{to: "张三", text: "周末我有空"}
	rec := &fakeRecorder{}
	att := &fakeAttention{backlog: []string{"张三: 周末去看展吗"}}
	a := newTestAgentWith(t, Options{
		Name: "noout", States: MVPStates(), Initial: "scrolling_phone",
		Chatter: chatter, Attention: att, Recorder: rec,
	})
	ctx := context.Background()
	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}
	if len(rec.got) != 1 {
		t.Fatalf("没有 outbox 时回复仍应进记忆，实际 %v", rec.got)
	}
	if got := streamJoined(a); !strings.Contains(got, "周末我有空") {
		t.Errorf("没有 outbox 时回复仍应进意识流:\n%s", got)
	}
}

// TestSendMessageToWhomUsesOwnWords 验证 to 为空时的措辞不串味。
func TestSendMessageToWhomUsesOwnWords(t *testing.T) {
	chatter := &sendChatter{text: "好"}
	rec := &fakeRecorder{}
	att := &fakeAttention{backlog: []string{"张三: 在吗"}}
	a := newTestAgentWith(t, Options{
		Name: "noto", States: MVPStates(), Initial: "scrolling_phone",
		Chatter: chatter, Attention: att, Recorder: rec,
	})
	ctx := context.Background()
	for i := 0; i < 12 && a.LastRecord().Reason == ""; i++ {
		stepSync(t, a, ctx)
	}
	if len(rec.got) != 1 {
		t.Fatalf("应写入 1 条，实际 %v", rec.got)
	}
	if !strings.HasPrefix(rec.got[0], "回了句：") {
		t.Errorf("没指定收件人时的记法 = %q", rec.got[0])
	}
}
