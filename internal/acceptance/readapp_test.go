package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Sparrived/SiriusRealLife/internal/fsm"
)

// readAppChatter 会先"翻一眼"再决定，用于走通 read_app 那条路。
//
// 三种调用按形状区分：
//   - 带 Schema：独白轮，只回一段内心活动；
//   - 带 send_message：工具结果轮，把它记下来——**记忆只在这一轮注入**。
//     它带 send_message 但不带状态工具，所以**不能**按"有没有工具"判轮次；
//   - 带 stay/enter_state：状态决策轮，第一次先调 read_app，之后正常做决定。
type readAppChatter struct {
	read            bool
	toolReadPrompts []string
}

func (c *readAppChatter) Chat(_ context.Context, req fsm.ChatRequest) (fsm.ChatResponse, error) {
	if req.Schema != nil {
		return fsm.ChatResponse{
			Text: `{"thought":"有点想看看展览的事","action":"","intent":"翻翻展览的聊天记录"}`,
		}, nil
	}
	if hasToolSpec(req.Tools, "send_message") {
		c.toolReadPrompts = append(c.toolReadPrompts, req.Prompt)
		return fsm.ChatResponse{Text: "看明白了，回头再回"}, nil
	}
	if !c.read && hasToolSpec(req.Tools, "read_app") {
		c.read = true
		return fsm.ChatResponse{
			Text: "先翻翻看",
			ToolCalls: []fsm.ToolCall{{
				ID: "r1", Name: "read_app",
				Arguments: json.RawMessage(`{"app":"qq","n":5}`),
			}},
		}, nil
	}
	return fsm.ChatResponse{
		Text: "翻完了，先这样",
		ToolCalls: []fsm.ToolCall{{
			ID: "c1", Name: "stay",
			Arguments: json.RawMessage(`{"why":"翻完了，先这样","for_ticks":5}`),
		}},
	}, nil
}

func hasToolSpec(specs []fsm.ToolSpec, name string) bool {
	for _, s := range specs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestAcceptanceReadAppTurnCarriesMemory 验收整条"先看一眼再决定"的链路。
//
// 这条测试盯的是一件**只有跨模块才成立**的事：打捞只在工具结果轮发生，
// 而那一轮要拿到的，是**记忆层**用刚读到的内容捞回来的东西。fsm 单测
// 用假 Dredge 只能证明"框架在那一轮调了打捞"，证明不了真实 Store 里
// 捞得出东西——那需要内容先被写入待选区，而这依赖 read_app → Browse →
// rememberSeen 一整条接线。
//
// 因此断言全部走真实入口：消息从 HTTP /events 进，读由 read_app 经
// 真实 Store 翻页，打捞经真实 Store.Dredge，快照经 HTTP GET 读出来。
func TestAcceptanceReadAppTurnCarriesMemory(t *testing.T) {
	chatter := &readAppChatter{}
	h := newHarness(t, "scrolling_phone", chatter)
	ctx := context.Background()

	// 消息经真实 HTTP 入口投递。
	for _, m := range []string{
		`{"from":"张三","text":"周末去看展吗"}`,
		`{"from":"李四","text":"听说那个展挺好的"}`,
	} {
		resp, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
			"application/json", strings.NewReader(m))
		if err != nil {
			t.Fatalf("POST 事件: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("投递事件状态码 = %d", resp.StatusCode)
		}
	}

	// 推到"翻一眼 → 工具结果轮 → 重新决策"整条走完。
	for i := 0; i < 25; i++ {
		h.agent.Step(ctx)
		settle(t, h, ctx)
	}

	if len(chatter.toolReadPrompts) == 0 {
		t.Fatal("没有发生工具结果轮——read_app 那条路没走通")
	}
	got := chatter.toolReadPrompts[0]

	// 关键断言 1：那一轮真的带上了打捞回来的记忆。
	// 若失败，说明打捞没有发生在工具结果轮（或在那一轮捞不到东西）。
	if !strings.Contains(got, "【想起的事】") {
		t.Errorf("工具结果轮里没有打捞回来的记忆：\n%s", got)
	}
	// 关键断言 2：刚翻到的内容也在（记忆是"围绕它"捞的）。
	if !strings.Contains(got, "周末去看展吗") {
		t.Errorf("工具结果轮里没有刚翻到的内容：\n%s", got)
	}

	// 关键断言 3：翻到的内容写进了待选区（经 HTTP 观测）。
	if mem := h.snapshotMemory(t); mem.Staging == 0 {
		t.Error("翻到的内容应写入待选区，memory.staging 仍为 0")
	}
}

// replyChatter 会走完整条"翻一眼 → 回一句 → 再决定"。
type replyChatter struct {
	read bool
	text string
	// sentTo 记录它实际回了谁，供断言。
	sentTo string
}

func (c *replyChatter) Chat(_ context.Context, req fsm.ChatRequest) (fsm.ChatResponse, error) {
	if req.Schema != nil {
		return fsm.ChatResponse{
			Text: `{"thought":"王五在问我展览的名字","action":"","intent":"翻记录找展览名字"}`,
		}, nil
	}
	// 工具结果轮的特征就是带着 send_message。
	if hasToolSpec(req.Tools, "send_message") {
		c.sentTo = "王五"
		args, _ := json.Marshal(map[string]any{"to": c.sentTo, "text": c.text})
		return fsm.ChatResponse{
			Text:      "我跟他说一声",
			ToolCalls: []fsm.ToolCall{{ID: "s1", Name: "send_message", Arguments: args}},
		}, nil
	}
	if !c.read && hasToolSpec(req.Tools, "read_app") {
		c.read = true
		return fsm.ChatResponse{
			Text: "先翻翻看",
			ToolCalls: []fsm.ToolCall{{
				ID: "r1", Name: "read_app",
				Arguments: json.RawMessage(`{"app":"qq","n":5}`),
			}},
		}, nil
	}
	return fsm.ChatResponse{
		Text: "回完了，接着干",
		ToolCalls: []fsm.ToolCall{{
			ID: "c1", Name: "stay",
			Arguments: json.RawMessage(`{"why":"回完了","for_ticks":5}`),
		}},
	}, nil
}

// TestAcceptanceReplyEntersMemory 验收"说出去的话进记忆"这条路。
//
// 走真实入口：消息从 HTTP /events 进，读经真实 Store、回复经真实
// Recorder 写回同一个 Store，最后在快照与存储里核对。
//
// 这条必须端到端测，因为它是**跨模块**才成立的：fsm 侧只用假 Recorder
// 证明得了"框架在回复时调了记录"，证明不了真实 Store 把这句话写进了
// 那一段翻阅——而那正是日后能"想起这段对话"的前提。
func TestAcceptanceReplyEntersMemory(t *testing.T) {
	const reply = "那个展叫「无尽的素描」"
	chatter := &replyChatter{text: reply}
	h := newHarness(t, "scrolling_phone", chatter)
	ctx := context.Background()

	for _, m := range []string{
		`{"from":"张三","text":"周末去看展吗"}`,
		`{"from":"王五","text":"@你 上次说的展览叫啥","mentions_me":true}`,
	} {
		resp, err := http.Post(h.server.URL+"/api/v1/agents/sirius/events",
			"application/json", strings.NewReader(m))
		if err != nil {
			t.Fatalf("POST 事件: %v", err)
		}
		resp.Body.Close()
	}

	for i := 0; i < 25; i++ {
		h.agent.Step(ctx)
		settle(t, h, ctx)
	}

	if chatter.sentTo == "" {
		t.Fatal("模型没有走 send_message——回复路径没被触发")
	}

	// 1) 她说的话进了意识流，类型是**动作**（做过的事，不是想法）。
	var found *fsm.StreamEntry
	for i := range h.agent.Stream {
		if strings.Contains(h.agent.Stream[i].Text, reply) {
			found = &h.agent.Stream[i]
		}
	}
	if found == nil {
		t.Fatalf("回复没进意识流")
	}
	if found.Kind != fsm.KindAction {
		t.Errorf("回复应以动作类型入意识流，实际 %v：%q", found.Kind, found.Text)
	}

	// 2) 她说的话进了**记忆层**（关键：跨模块，只有真实 Store 能证明）。
	var inMemory bool
	for _, e := range h.store.Staging() {
		if strings.Contains(e.Text, reply) {
			inMemory = true
		}
	}
	if !inMemory {
		t.Error("回复没写进待选区——说出去的话必须进记忆")
	}

	// 3) 经 HTTP 观测：待选区确实有东西（staging 为 0 即链路断了）。
	if mem := h.snapshotMemory(t); mem.Staging == 0 {
		t.Error("memory.staging 为 0，记忆写入链路断了")
	}
}
