package fsm

// Candidate 是一个候选状态，或一个被挡掉的状态及原因。
//
// 与旧版的区别：不再有权重。状态不再"按概率被抽中"，而是全部交给 LLM
// 选（R3 修订）。因此快照要记录的是**当时摆在模型面前的是什么**，
// 以及**哪些被框架挡住了、为什么**——后者是排障时最常问的问题
// （"为什么她从来不去睡觉？"），而它不能靠事后推断，因为 Guard
// 依赖于当时的心境与 tick。
type Candidate struct {
	Name StateName
	// Blocked 为空表示它是候选；非空是它没进候选的原因（给人看的中文）。
	Blocked string
}

// DispatchRecord 是一次状态决定的完整记录（R6）。
//
// 没有随机数，因此也没有 roll/total：决定的依据是 LLM 说的话
// （Why）与它看到的选择（Candidates）。复现靠这条日志，不靠种子。
type DispatchRecord struct {
	From StateName
	To   StateName
	// Reason 是决定的来源：llm / stay。
	//
	//	llm  —— LLM 选了另一个状态（这是唯一的"转移"）
	//	stay —— 继续留在原状态（LLM 主动选择，或调用失败后的降级）
	//
	// 刻意不再有 preempt：抢占整个取消后，没有任何框架来源能改变状态
	// （见 §2.1）。框架只提供理由（Trigger），换不换由 LLM 说了算。
	Reason string
	// Why 是 LLM 给的理由；Reason 为 max/invalid 时是框架给的理由。
	Why string
	// ForTicks 是这次决定的停留时长（夹紧之后的实际值）。
	ForTicks Tick
	// Candidates 是当时摆给模型看的完整清单（含被挡掉的）。
	Candidates []Candidate
	Seq        Tick
}

// menu 返回当前允许进入的状态名（Guard 通过、不在冷却、不是当前状态）。
//
// 当前状态**刻意不在菜单里**：想继续待着应当调 stay，那是另一件事，
// 有自己的理由与时长。
func (r DispatchRecord) menu() []StateName {
	var out []StateName
	for _, c := range r.Candidates {
		if c.Blocked == "" {
			out = append(out, c.Name)
		}
	}
	return out
}

// survey 盘点所有状态此刻的可选性，形成一张完整清单。
//
// 这是框架对 LLM 的**唯一**约束入口：模型能看到的候选就是这里
// Blocked 为空的那些，因此它不可能选到一个此刻不能进的状态。
// 约束在这里生效，而不是靠 prompt 里写"请不要选某某"。
func (a *Agent) survey() []Candidate {
	out := make([]Candidate, 0, len(a.order))
	for _, s := range a.order {
		switch {
		case s.Name == a.Current:
			out = append(out, Candidate{Name: s.Name, Blocked: "就是现在待着的状态（想继续就用 stay）"})
		case s.Guard != nil && !s.Guard(a):
			out = append(out, Candidate{Name: s.Name, Blocked: "此刻不满足进入条件"})
		case a.inCooldown(s.Name):
			out = append(out, Candidate{Name: s.Name, Blocked: "刚从这里离开不久"})
		default:
			out = append(out, Candidate{Name: s.Name})
		}
	}
	return out
}

// inCooldown 报告某状态是否还在冷却期内。
func (a *Agent) inCooldown(name StateName) bool {
	until, ok := a.cooldownUntil[name]
	return ok && a.Now < until
}

// stayRecord 造一条"继续待着"的决定记录。
//
// To 与 From 相同：stay 的语义就是"状态没变，但检查点推后了"。
// 这不是一次转移，因此 R6 日志的 reason 用 stay 与新状态区分开。
func (a *Agent) stayRecord(why string, forTicks Tick) DispatchRecord {
	return DispatchRecord{
		From: a.Current, To: a.Current, Reason: "stay", Why: why,
		ForTicks: forTicks, Candidates: a.survey(), Seq: a.Now,
	}
}
