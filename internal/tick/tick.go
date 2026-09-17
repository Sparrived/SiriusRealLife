// Package tick 是**唯一**做时间换算的地方（R8）。
//
// 业务逻辑只认 tick 序号，不认 time.Now()。真实时间到游戏时间的映射
// 只在这里发生一次，离线推演、加速、回放因此都能免费拿到。
package tick

import (
	"context"
	"time"
)

// Source 产生 tick 信号。
type Source struct {
	// Interval 是每个 tick 的真实时长。为 0 表示**不等待**（压测/离线回放）。
	Interval time.Duration
}

// Run 按 Interval 往 out 发送信号，直到 ctx 结束。它会关闭 out。
//
// Interval 为 0 时全速执行——30-tick 验收用它跑确定性测试，
// 不受真实时间影响。
func (s Source) Run(ctx context.Context, out chan<- struct{}) {
	defer close(out)
	if s.Interval <= 0 {
		for {
			select {
			case <-ctx.Done():
				return
			case out <- struct{}{}:
			}
		}
	}

	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case out <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}
