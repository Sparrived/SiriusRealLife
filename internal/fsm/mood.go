// Package fsm 实现人格的状态机核心：状态定义、加权分派、tick 循环。
//
// 设计约束（见 AGENTS.md 硬规则）：
//   - R1 一个 agent 一个 goroutine，状态只被它自己改
//   - R2 状态与心境正交，影响方向单向
//   - R3 加权抽取、不可复现的随机、同状态不连续进入
//   - R4 LLM 调用是显式状态，不阻塞 tick
//   - R6 每次转移留结构化日志
//   - R8 业务逻辑只认 tick 序号，不认 time.Now()
package fsm

// Tick 是唯一的时间单位。业务逻辑只认它（R8）。
type Tick int64

// Mood 是连续量心境，随时间衰减并与状态正交（R2）。
//
// 取值范围约定为 [0, 100]。它不是状态的一部分：状态只能改它，
// 它只能影响分派权重与 prompt，不能反向决定"当前是哪个状态"。
type Mood struct {
	Energy  float64 // 精力
	Annoyed float64 // 烦躁
	Curious float64 // 好奇
}

// Clamp 把各分量夹到 [0, 100]，防止衰减或事件把心境推出合法区间。
func (m *Mood) Clamp() {
	for _, p := range []*float64{&m.Energy, &m.Annoyed, &m.Curious} {
		if *p < 0 {
			*p = 0
		}
		if *p > 100 {
			*p = 100
		}
	}
}
