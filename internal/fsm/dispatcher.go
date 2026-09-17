package fsm

import (
	"fmt"
)

// Candidate 是一次分派尝试中的候选项，连同它的实际权重。
//
// 权重快照要进 R6 日志：出问题时唯一能复盘"为什么选了这个状态"的东西。
type Candidate struct {
	Name   StateName
	Weight float64
}

// DispatchRecord 是一次分派的完整记录，直接对应 R6 要求的结构化日志。
type DispatchRecord struct {
	From       StateName
	To         StateName
	Reason     string // timeout / event / dispatch / preempt
	Candidates []Candidate
	Roll       float64 // 抽出的随机值
	Total      float64 // 权重总和
	Seq        Tick
}

// dispatch 按权重抽取下一个状态（R3）。
//
// 规则：
//  1. 过滤 Guard 为假、尚在 Cooldown 内的状态
//  2. 排除当前状态（同状态不允许连续进入）
//  3. 用当前心境调整权重
//  4. 按权重抽取
//
// 随机数来自 agent 自己的 *rand.Rand（R3 可复现），不用全局 math/rand。
func (a *Agent) dispatch() (StateName, DispatchRecord, error) {
	rec := DispatchRecord{From: a.Current, Reason: "dispatch", Seq: a.Now}

	var cands []Candidate
	var total float64
	for _, s := range a.order {
		if s.Name == a.Current {
			continue // 规则 2：不连续进入同一状态
		}
		if s.Guard != nil && !s.Guard(a) {
			continue
		}
		if cd, ok := a.cooldownUntil[s.Name]; ok && a.Now < cd {
			continue
		}
		w := s.moodWeight(a)
		if w <= 0 {
			continue
		}
		cands = append(cands, Candidate{Name: s.Name, Weight: w})
		total += w
	}

	if len(cands) == 0 {
		return "", rec, fmt.Errorf("fsm: 无候选状态（current=%s seq=%d）", a.Current, a.Now)
	}

	// 抽取：roll ∈ [0,total)，累减直到落到某个候选。
	roll := a.rng.Float64() * total
	var acc float64
	chosen := cands[len(cands)-1].Name
	for _, c := range cands {
		acc += c.Weight
		if roll < acc {
			chosen = c.Name
			break
		}
	}

	rec.To = chosen
	rec.Candidates = cands
	rec.Roll = roll
	rec.Total = total
	return chosen, rec, nil
}
