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

// MoodRates 是心境每 tick 的衰减速率（"人格参数"，应按人格调）。
//
// 单位是"每 tick 掉多少点"，取值区间 [0,100]。
// 默认值把量级按**天**校准：精力从满到空约一天，烦躁约 4 小时，
// 好奇约一天半。这些数字直接决定人格"像不像人"，因此显式放在
// 状态/人格参数层，而不是散落在 decay 函数里当字面量。
type MoodRates struct {
	Energy  float64
	Annoyed float64
	Curious float64
}

// DefaultMoodRates 是默认衰减率。
//
// 精力 100/1440：恰好一天（24 游戏小时）从满掉到空。这个数字由
// "收支平衡"定出来，不是拍脑袋：唯一回复手段是睡觉
// （sleeping.OnExit → Energy=95），若日衰减超过睡觉能补回的量，
// 精力会长期钉在 0，人格就失去"累"的层次。实测（15 种子 × 14 天）：
//
//	衰减        精力为0的时间占比   最长连续为0
//	1/720            26.1%          9.8 游戏小时
//	1/960             4.6%          5.3
//	1/1200            0.1%          2.4
//	1/1440            0.0%          0.9   ← 采用
//	1/1920            0.0%          0.0   （睡太少，日均 1.3 次）
//
// 烦躁 100/240（4 小时消气）；好奇 100/1440（一天半）。
// 改这里必须重跑 TestMoodEconomyBalanced。
func DefaultMoodRates() MoodRates {
	return MoodRates{
		Energy:  100.0 / 1440.0,
		Annoyed: 100.0 / 240.0,
		Curious: 100.0 / 1440.0,
	}
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
