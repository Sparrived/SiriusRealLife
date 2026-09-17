package fsm

// 时间换算（R8）：真实时间与游戏时间的换算只在 tick 源头做一次。
// 业务逻辑只认 tick 序号；下面这些函数是"tick → 游戏时刻"的唯一换算处。
//
// 取值：1 tick = 1 游戏分钟，一天 1440 tick。这个比例是约定值，
// 改它只需要改这一处；状态表里的 minTick/maxTick 也都是 tick 数。

// TicksPerDay 是一天的 tick 数（1 tick = 1 游戏分钟）。
const TicksPerDay Tick = 24 * 60

// Hour 返回某个 tick 对应的游戏小时（0–23）。
func Hour(t Tick) int {
	m := t % TicksPerDay
	if m < 0 {
		m += TicksPerDay
	}
	return int(m / 60)
}
