package fsm

import "encoding/json"

// ToolCall 是一次工具调用。
//
// 为什么用具名结构而不是 map：工具名与参数是**状态机的输入**，
// 决定下一步进入哪个状态。用 map 会让"取参数"散落成一片类型断言，
// 而这里正是最需要确定性的地方。
type ToolCall struct {
	// ID 由上游给出。单轮决策用不到它，但留着便于排障对照上游日志。
	ID string
	// Name 是工具名，对应 ToolSpec.Name。
	Name string
	// Arguments 是**未解析**的参数 JSON。
	//
	// 解析由各工具自己做：fsm 不该知道 read_app 的参数形状（R7）。
	// 上游把它作为字符串传输，llm 侧已经转成 RawMessage（空串补成 {}）。
	Arguments json.RawMessage
}

// ToolSpec 是一个工具的声明，送给 LLM 用于选择。
//
// Parameters 是 JSON Schema（object 根）。声明与实现分开：spec 给模型看，
// 执行给框架做（R7）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}
