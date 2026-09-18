package fsm

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestParseMonologue 验证独白解析：带前缀的行分类型，无前缀的行兜底成想法。
//
// 这条解析是意识流"有类型"的唯一来源，且必须**坏得比较轻**：
// 模型多写一句解释时，宁可类型不准，也不能把整段内容丢掉。
func TestParseMonologue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Kind
		text []string
	}{
		{
			name: "三种前缀",
			in:   "想: 有点无聊\n做: 拿起手机\n打算: 待会去写点东西",
			want: []Kind{KindThought, KindAction, KindIntent},
			text: []string{"有点无聊", "拿起手机", "待会去写点东西"},
		},
		{
			name: "无前缀兜底成想法",
			in:   "今天什么都不想干",
			want: []Kind{KindThought},
			text: []string{"今天什么都不想干"},
		},
		{
			name: "混合：无前缀行不丢",
			in:   "想: 好累\n（这句是模型多写的解释）\n打算: 早点睡",
			want: []Kind{KindThought, KindThought, KindIntent},
			text: []string{"好累", "（这句是模型多写的解释）", "早点睡"},
		},
		{
			name: "空行被跳过",
			in:   "\n\n想: 只有这一句\n\n",
			want: []Kind{KindThought},
			text: []string{"只有这一句"},
		},
		{
			name: "前缀后为空则丢弃该行",
			in:   "想:\n想: 有内容",
			want: []Kind{KindThought},
			text: []string{"有内容"},
		},
		{
			// 实测：同一 prompt 下 AMKR 的 auto 路由回全角冒号。
			// 只认半角会让意图被静默降级成普通想法。
			name: "全角冒号",
			in:   "想： 有点无聊\n打算： 去写点东西",
			want: []Kind{KindThought, KindIntent},
			text: []string{"有点无聊", "去写点东西"},
		},
		{
			// 实测：模型会加粗标签。
			name: "markdown 加粗标签",
			in:   "**想：** 有点困\n**打算：** 早点睡",
			want: []Kind{KindThought, KindIntent},
			text: []string{"有点困", "早点睡"},
		},
		{
			// "想做点什么" 不能被切成 "想" + "做点什么"。
			name: "标签后无冒号则不作前缀",
			in:   "想做点什么但没想好",
			want: []Kind{KindThought},
			text: []string{"想做点什么但没想好"},
		},
		{
			name: "JSON 三字段",
			in:   `{"thought":"有点无聊","action":"刷手机","intent":"去写代码"}`,
			want: []Kind{KindThought, KindAction, KindIntent},
			text: []string{"有点无聊", "刷手机", "去写代码"},
		},
		{
			// 实测：模型常把 JSON 包进 markdown 围栏。
			name: "JSON 带代码围栏",
			in:   "```json\n{\"thought\":\"有点无聊\",\"action\":\"\",\"intent\":\"去写代码\"}\n```",
			want: []Kind{KindThought, KindIntent},
			text: []string{"有点无聊", "去写代码"},
		},
		{
			// 实测：json_object 模式（或忽略 schema 的路由）下模型
			// 自起字段名，deepseek 回了"内心活动"。
			name: "JSON 字段名别名",
			in:   `{"内心活动":"有点无聊"}`,
			want: []Kind{KindThought},
			text: []string{"有点无聊"},
		},
		{
			name: "JSON answer 别名",
			in:   `{"answer":"发会呆吧"}`,
			want: []Kind{KindThought},
			text: []string{"发会呆吧"},
		},
		{
			// 合法 JSON 但字段全不认识：不当作 JSON 消费，
			// 交给文本兜底，内容才不会被丢掉。
			name: "JSON 无可用字段则走文本兜底",
			in:   `{"foo":"这段内容不该消失"}`,
			want: []Kind{KindThought},
			text: []string{`{"foo":"这段内容不该消失"}`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseMonologue(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("条数 = %d (%v), 期望 %d", len(got), got, len(c.want))
			}
			for i := range got {
				if got[i].Kind != c.want[i] {
					t.Errorf("第 %d 条 Kind = %v, 期望 %v", i, got[i].Kind, c.want[i])
				}
				if got[i].Text != c.text[i] {
					t.Errorf("第 %d 条 Text = %q, 期望 %q", i, got[i].Text, c.text[i])
				}
			}
		})
	}

	// 空回复不产生任何记录：调用方据此不写流水账。
	if got := parseMonologue("   \n  "); got != nil {
		t.Errorf("空回复应返回 nil, 得到 %v", got)
	}
}

// TestIntentSurvivesStateChange 验证"接下来打算做什么"跨状态存活。
//
// 这是修复前后最关键的差别：意图过去不存在，换状态后 agent 就"忘了
// 自己要干什么"。它是**日志里的一条记录**，因此不会被状态切换清掉。
func TestIntentSurvivesStateChange(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	ctx := context.Background()

	a.appendStreamKind(KindIntent, "待会把那段代码写完")
	if got := a.Intent(); got != "待会把那段代码写完" {
		t.Fatalf("Intent = %q", got)
	}

	// 强制换几次状态：意图必须还在。
	for i := 0; i < 60; i++ {
		a.Step(ctx)
		if got := a.Intent(); got != "待会把那段代码写完" {
			t.Fatalf("第 %d tick（状态 %s）后意图丢失，得到 %q", i, a.Current, got)
		}
	}
}

// TestIntentIsLatest 验证取的是**最近**一条意图，不是第一条。
func TestIntentIsLatest(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.appendStreamKind(KindIntent, "先吃饭")
	a.appendStreamKind(KindThought, "吃什么呢")
	a.appendStreamKind(KindIntent, "还是先写代码")

	if got := a.Intent(); got != "还是先写代码" {
		t.Fatalf("Intent = %q, 期望最新一条", got)
	}

	// 没有意图时返回空串，而不是报错或返回脏数据。
	b := newTestAgent(t, fakeChatter{})
	if got := b.Intent(); got != "" {
		t.Fatalf("无意图时 Intent = %q, 期望空", got)
	}
}

// TestContextSections 验证上下文装配：该有的段落都在，且按类型分开。
//
// R5 的落点：prompt 只读**有界的**子集，绝不塞全量意识流。
func TestContextSections(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.appendStreamKind(KindObservation, "张三: 在吗")
	a.appendStreamKind(KindThought, "有点困")
	a.appendStreamKind(KindAction, "拿起手机刷一刷")
	a.appendStreamKind(KindIntent, "待会去写点东西")

	got := a.Context(SiteMonologue, ContextOptions{SelfModel: []string{"我叫 Sirius"}})

	for _, want := range []string{
		"【现在】", "【心境】", "【我是谁】我叫 Sirius",
		"【最近在想】", "有点困",
		"【刚发生】", "张三: 在吗",
		"【打算】待会去写点东西",
		"【适合做】",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("上下文缺少 %q\n---\n%s", want, got)
		}
	}

	// 类型必须真的分开：想法不该出现在"刚发生"里。
	recent := got[strings.Index(got, "【刚发生】"):]
	if end := strings.Index(recent, "\n"); end >= 0 {
		recent = recent[:end]
	}
	if strings.Contains(recent, "有点困") {
		t.Errorf("想法不应出现在【刚发生】段: %s", recent)
	}
}

// TestContextReportsUnread 验证"有未读消息"作为**持续事实**进入 prompt。
//
// 这条守的是一个很容易漏掉的缺口：消息到达时意识流里写的那句"有人叫我"
// 只在到达那一刻写一次，而【刚发生】只取最近 8 条。一条 20 tick 前到的
// 消息会被挤出窗口，此后模型再也看不到"还有没看的消息"——它就没法产生
// "要不要去看看手机"这个念头，只能靠碰巧又来了新消息。
//
// 未读数是持续存在的事实，必须每轮都告知（这也是"如果 QQ 来消息了，
// 应当在提示词告知"的落点）。同时它**不是**触发条件（R12）：说了不等于
// 框架去换状态，只是让模型知道。
func TestContextReportsUnread(t *testing.T) {
	a, quit := newAgentWithFakeAttention(t) // 队列里 2 条未读
	defer quit()

	got := a.Context(SiteDispatch, ContextOptions{})
	if !strings.Contains(got, "【手机】") || !strings.Contains(got, "2 条") {
		t.Errorf("有未读时上下文应报告未读条数：\n%s", got)
	}

	// 读完（游标推进）之后整段应当消失，而不是写"0 条"。
	a.attention.Scan(10)
	if got := a.Context(SiteDispatch, ContextOptions{}); strings.Contains(got, "【手机】") {
		t.Errorf("没有未读时不该出现【手机】段：\n%s", got)
	}
}

// TestContextReportsUnreadMentions 验证 @ 在没看手机时也有痕迹。
//
// 这是用户明确要求复刻的 QQ 语义："@ 和回复这两种模式在 QQ 中的反应
// 就是会显式提醒到用户有人提及你，而一般消息没有专门的提及"。
//
// 抢占取消后（R12），@ 与普通消息的**唯一**差别必须在某处落地。
// 若只报"还有 N 条没看"，那么"其中一条是专门叫你的"就丢了——
// 模型没法把"有人叫我"和"群里又在灌水"区分开，@ 等于不存在。
func TestContextReportsUnreadMentions(t *testing.T) {
	a, quit := newAgentWithFakeAttention(t)
	defer quit()

	// 默认没有提及：不该凭空写出"提到了你"。
	if got := a.Context(SiteDispatch, ContextOptions{}); strings.Contains(got, "提到了你") {
		t.Errorf("没有被 @ 时不该报告提及：\n%s", got)
	}

	// 有 1 条 @我：必须显式点出来。
	a.attention.(*fakeAttention).mentions = 1
	got := a.Context(SiteDispatch, ContextOptions{})
	if !strings.Contains(got, "【手机】") || !strings.Contains(got, "提到了你") {
		t.Errorf("@我的消息应当在【手机】段里被显式点出：\n%s", got)
	}
}

// TestContextRespectsLimits 验证每段都有上限（R5）。
func TestContextRespectsLimits(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	for i := 0; i < 40; i++ {
		a.appendStreamKind(KindThought, "想法"+string(rune('A'+i%26)))
	}

	got := a.Context(SiteMonologue, ContextOptions{Limits: ContextLimits{Thoughts: 3, Recent: 2}})

	line := got[strings.Index(got, "【最近在想】"):]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	// 3 条想法 + 2 个分隔符 "/ " 之间的分隔符数。
	if n := strings.Count(line, " / "); n != 2 {
		t.Errorf("【最近在想】应有 3 条（2 个分隔符），得到 %d 个: %s", n, line)
	}
}

// TestContextDredgeIncluded 验证打捞结果进入 prompt（memory.md §5.1）。
func TestContextDredgeIncluded(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.appendStreamKind(KindIntent, "把那段代码写完")
	a.pendingRead = []string{"张三: 周末去看展吗"}

	var asked []string
	got := a.Context(SiteToolRead, ContextOptions{
		Dredge: func(query []string, now Tick) []string {
			asked = query
			return []string{"上周也差不多是这个进度"}
		},
	})
	if !strings.Contains(got, "【想起的事】上周也差不多是这个进度") {
		t.Errorf("打捞结果未进上下文:\n%s", got)
	}
	// 查询词首项应当是**刚读到的内容**：要回忆的是"这段内容让我想起什么"。
	if len(asked) == 0 || asked[0] != "张三: 周末去看展吗" {
		t.Errorf("打捞查询词 = %v, 期望首项是刚读到的内容", asked)
	}
	// 意图也要在里面，让联想不完全被眼前这条消息绑架。
	if !slices.Contains(asked, "把那段代码写完") {
		t.Errorf("打捞查询词 = %v, 期望含当前意图", asked)
	}
	// now 必须被传下去（记忆曲线刷新只认 tick, R8）。
	if got := a.Context(SiteToolRead, ContextOptions{
		Dredge: func(_ []string, now Tick) []string {
			if now != a.Now {
				t.Errorf("打捞收到 tick = %d, 期望 %d", now, a.Now)
			}
			return nil
		},
	}); got == "" {
		t.Fatal("上下文为空")
	}
}

// TestDredgeOnlyAtToolRead 验证打捞只在**工具结果轮**发生。
//
// 打捞的用途只有一个：为了行动而回忆。刚读到内容、还没决定怎么回应
// 的那一刻才需要它——回复一条消息得先想起之前跟这个人聊过什么。
// 独白是在想、决策点还没拿到内容，两者都不该打捞。
//
// 更要紧的是打捞**有副作用**（刷新记忆曲线、累计升格次数）：挂在每次
// 装配 prompt 上，等于把每次装配都算成一次"反复想起"，普通群聊会被
// 迅速顶成事件记忆（线上实测过 ~40 tick 就发生）。
func TestDredgeOnlyAtToolRead(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.appendStreamKind(KindIntent, "把那段代码写完")
	a.pendingRead = []string{"张三: 周末去看展吗"}

	calls := 0
	opt := ContextOptions{
		Dredge: func([]string, Tick) []string {
			calls++
			return []string{"以前也聊过这个"}
		},
	}
	for _, site := range []CallSite{SiteMonologue, SiteDispatch} {
		got := a.Context(site, opt)
		if strings.Contains(got, "【想起的事】") {
			t.Errorf("%s 不该打捞:\n%s", site, got)
		}
	}
	if calls != 0 {
		t.Errorf("非工具轮不该调用打捞函数（它有副作用），实际调了 %d 次", calls)
	}

	got := a.Context(SiteToolRead, opt)
	if !strings.Contains(got, "【想起的事】以前也聊过这个") {
		t.Errorf("工具结果轮必须打捞:\n%s", got)
	}
	if calls != 1 {
		t.Errorf("工具结果轮应当正好调用一次打捞，实际 %d 次", calls)
	}
}

// TestContextDiffersBySite 验证三个调用点的追问不同。
//
// 上下文共享、追问各异——这正是"不是一个统一大 prompt"的落点。
func TestContextDiffersBySite(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	sites := []CallSite{SiteMonologue, SiteDispatch, SiteToolRead}
	seen := map[string]CallSite{}
	for _, s := range sites {
		got := a.Context(s, ContextOptions{})
		if prev, dup := seen[got]; dup {
			t.Errorf("调用点 %s 与 %s 产出了完全相同的上下文", s, prev)
		}
		seen[got] = s
	}
}

// TestMonologueWritesTypedEntries 验证独白结果真正变成带类型的意识流记录。
//
// 这条是整条链路的验收：LLM 说了什么 → 解析 → 分类型 → 落进意识流。
func TestMonologueWritesTypedEntries(t *testing.T) {
	a := newTestAgent(t, fakeChatter{reply: "想: 有点无聊\n打算: 去写点东西"})
	before := len(a.Stream)

	a.absorbNarration("想: 有点无聊\n打算: 去写点东西")

	added := a.Stream[before:]
	if len(added) != 2 || added[0].Kind != KindThought || added[1].Kind != KindIntent {
		t.Fatalf("新增记录 = %+v, 期望 [thought intent]", added)
	}
	if added[0].Text != "有点无聊" || added[1].Text != "去写点东西" {
		t.Fatalf("记录文本 = %q / %q", added[0].Text, added[1].Text)
	}
	if got := a.Intent(); got != "去写点东西" {
		t.Fatalf("Intent = %q", got)
	}
	// 结果必须带上 tick 与状态，前端才排得出时间线。
	if added[0].Seq != a.Now {
		t.Errorf("Seq = %d, 期望 %d", added[0].Seq, a.Now)
	}
}

// TestMonologueAttributedToOriginState 验证晚到的独白归给**发起它的**状态。
//
// 结果回来的时刻可能已经换过状态了。若按当前状态记，"刷手机时想的事"
// 会显示成"干活时想的事"，意识流就失真了。
func TestMonologueAttributedToOriginState(t *testing.T) {
	a := newTestAgent(t, fakeChatter{})
	a.monologueOn = true
	a.monologueEvery = -1 // 不节流
	a.inFlightState = "scrolling_phone"

	// 模拟结果回来前状态已经换了。
	a.Current = "working"
	a.absorbNarration("想: 刷到一条有意思的")

	last := a.Stream[len(a.Stream)-1]
	if last.State != "scrolling_phone" {
		t.Fatalf("独白被记到状态 %s, 期望归给发起时的 scrolling_phone", last.State)
	}
}

// TestMonologueThrottled 验证独白节流（否则每次进入状态都发一次调用）。
//
// 断言用 IsThinking() 而不是统计调用次数：Chat 是在 goroutine 里跑的，
// 计数会在测试读到它之后才增加，那样的断言是随机的。
func TestMonologueThrottled(t *testing.T) {
	a := newTestAgent(t, fakeChatter{delay: time.Second, reply: "想: 嗯"})
	a.monologueOn = true
	a.monologueEvery = 100

	// 对齐到"刚发过一次独白"。
	a.lastMonologueAt = a.Now
	a.monologueSent = true
	a.monologue()
	if a.IsThinking() {
		t.Fatal("间隔内不应发起调用")
	}

	// 推过间隔后应放行。
	a.Now += 101
	a.monologue()
	if !a.IsThinking() {
		t.Fatal("超过间隔后应发起调用")
	}

	// 清理在途调用，避免 goroutine 泄漏到后续断言。
	a.cancelThinking()
}

// TestMonologueSkippedWhenDisabled 验证未开启独白时不发任何调用。
func TestMonologueSkippedWhenDisabled(t *testing.T) {
	a := newTestAgent(t, fakeChatter{reply: "想: 嗯"})
	a.monologue() // Monologue 未开启
	if a.IsThinking() {
		t.Fatal("未开启独白时不应发起调用")
	}
}

// TestMessageIngested 验证外部消息进入记忆层，且**不**旁路 QQ 门控。
//
// 这是记忆链路的起点。同时验证消息正文不会直接落进意识流：
// §2.2 规定内容只在"看 QQ"状态被读取。
func TestMessageIngested(t *testing.T) {
	sink := &recordingSink{}
	a := newTestAgentWith(t, Options{
		Name: "test", States: MVPStates(), Initial: "working",
		Chatter: fakeChatter{}, Sink: sink,
	})
	ctx := context.Background()

	a.handleEvent(ctx, NewMessageEvent(IncomingMessage{
		From: "张三", Text: "在吗", MentionsMe: true,
	}))

	if len(sink.got) != 1 {
		t.Fatalf("记忆层收到 %d 条, 期望 1", len(sink.got))
	}
	if sink.got[0].Text != "在吗" || sink.got[0].From != "张三" {
		t.Errorf("记忆层收到的消息 = %+v", sink.got[0])
	}
	// Tick 必须被填上：记忆的时间轴只认 tick（R8）。
	if sink.got[0].Tick != a.Now {
		t.Errorf("消息 tick = %d, 期望 %d", sink.got[0].Tick, a.Now)
	}
	// QQ 门控：working 状态不该看到正文。
	for _, e := range a.Stream {
		if strings.Contains(e.Text, "在吗") {
			t.Errorf("消息正文旁路了 QQ 门控: %q", e.Text)
		}
	}
}

// TestMessageMarksMention 验证被 @ 时意识流留下提示（但不含正文）。
func TestMessageMarksMention(t *testing.T) {
	a := newTestAgentWith(t, Options{
		Name: "test", States: MVPStates(), Initial: "working",
		Chatter: fakeChatter{}, Sink: &recordingSink{},
	})
	a.handleEvent(context.Background(), NewMessageEvent(IncomingMessage{
		From: "张三", Text: "在吗", MentionsMe: true,
	}))

	var found bool
	for _, e := range a.Stream {
		if e.Kind == KindObservation && strings.Contains(e.Text, "叫我") {
			found = true
		}
	}
	if !found {
		t.Errorf("被 @ 后意识流应留下提示, 实际: %+v", a.Stream)
	}
}

// TestLLMFailureLoggedWithCause 验证 LLM 失败的原因进日志。
//
// 曾经这里只往意识流写一句"没想出来"，错误原文被丢掉。结果是容器里
// 所有独白静默失败，只看到满屏"话到嘴边没想出来"，得去翻上游 AMKR 的
// 日志文件才查得出是 unified-model 解析不到。意识流给 LLM 读人话，
// 日志给排障读原文，两者都要有（R10 说不重试，但没说可以看不见）。
func TestLLMFailureLoggedWithCause(t *testing.T) {
	var buf strings.Builder
	a := newTestAgentWith(t, Options{
		Name: "test", States: MVPStates(), Initial: "idle",
		Chatter: fakeChatter{},
		Logger:  slog.New(slog.NewTextHandler(&buf, nil)),
	})

	a.absorbLLM(context.Background(), nil) // 空负载：不该 panic
	a.handleEvent(context.Background(), Event{
		Kind: EventLLMFailed,
		Data: mustJSON(t, map[string]string{"error": "KeyError: 'unified-model'"}),
	})

	logged := buf.String()
	if !strings.Contains(logged, "llm_failed") {
		t.Errorf("失败未记进日志:\n%s", logged)
	}
	if !strings.Contains(logged, "unified-model") {
		t.Errorf("失败原因未进日志（排障就查不出来了）:\n%s", logged)
	}
	// 意识流里仍只写人话，不把上游错误灌给 LLM。
	if got := streamTextOf(a); strings.Contains(got, "KeyError") {
		t.Errorf("上游错误不该进意识流:\n%s", got)
	}
}

// TestStreamTextHasNoInternalIdentifiers 验证意识流里不出现内部标识。
//
// 意识流是唯一送给 LLM 读的文本。若把事件类型（user_message）这类内部
// 标识写进去，模型会看到自己不该看见的东西，人格叙事也脏了。
func TestStreamTextHasNoInternalIdentifiers(t *testing.T) {
	a := newTestAgentWith(t, Options{
		Name: "test", States: MVPStates(), Initial: "sleeping",
		Chatter: fakeChatter{}, Sink: &recordingSink{},
	})
	a.handleEvent(context.Background(), NewMessageEvent(IncomingMessage{
		From: "张三", Text: "在吗", MentionsMe: true,
	}))

	got := streamTextOf(a)
	for _, bad := range []string{"mention", "user_message", "llm_done", "llm_failed"} {
		if strings.Contains(got, bad) {
			t.Errorf("意识流出现内部标识 %q:\n%s", bad, got)
		}
	}
	// 但"有人叫我"这个事实必须留下。sleeping 不再有独立的延迟分支
	// （没有抢占就没有"延迟"），记录由 ingestMessage 统一写下。
	if !strings.Contains(got, "有人叫我") {
		t.Errorf("收到 @ 应留下记录:\n%s", got)
	}
}

// streamTextOf 拼接意识流全文，仅用于测试断言。
func streamTextOf(a *Agent) string {
	var b strings.Builder
	for _, e := range a.Stream {
		b.WriteString(e.Text)
		b.WriteString("\n")
	}
	return b.String()
}
