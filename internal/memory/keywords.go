package memory

import (
	"strings"
	"unicode"
)

// maxTokens 是单段文本最多产出多少个词元。
//
// 有界（R5）：打捞每次装配 prompt 都会跑，不能让它随文本长度线性膨胀。
// 256 对一条群消息、一句意图都远远够用。
const maxTokens = 256

// minGramHits 是一次打捞命中所需的**不同**词元重合数。
//
// 为什么不是"命中一个就算"：中文里"什么""时候""一下"这类二字组合
// 到处都是，只重合一个就会把无关的旧会话捞出来。要求两个不同的词元
// 同时重合，误命中会少很多，而真有关联的内容通常重合远不止两个。
//
// 查询很短时（例如模型/测试直接给一个词）阈值会降到词元总数，
// 否则"看展"这种两字查询永远命中不了——见 gramThreshold。
//
// ponytail: 硬编码常数。真觉得捞得不准就调它，或升级到 IDF 加权
// （词元在待选区里的文档频率，语料就在手边，不难算）。
const minGramHits = 2

// tokenize 把一段文本切成可匹配的词元。
//
// 这是打捞能work的前提。此前的实现是拿**整句**去做子串匹配
// （`strings.Contains(正文, "翻翻昨天聊过的记录打发时间")`），
// 而整句永远不可能是正文的子串——于是线上【想起的事】恒不出现，
// 但代码路径看着是通的（Dredge 有生产调用方），属于最难发现的那类缺口。
//
// 切法（不引任何分词库，纯标准库）：
//   - 连续汉字：产出全部 2-gram 与 3-gram。中文的信息密度高，
//     2 字已能表意（"看展""周报"），3 字更具体；单字太泛（"的""了"）直接丢。
//   - 连续拉丁字母/数字：整个词作为一个词元（"kanzhan" 不拆）。
//
// 去重、转小写，并滤掉一批纯功能词的组合（见 cjkStopGrams）。
func tokenize(s string) []string {
	var (
		out   []string
		seen  = map[string]bool{}
		cjk   []rune
		latin []rune
	)

	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] || cjkStopGrams[t] {
			return
		}
		// 单字不成词元：太泛，命中等于没命中。
		if len([]rune(t)) < 2 {
			return
		}
		if len(out) >= maxTokens {
			return
		}
		seen[t] = true
		out = append(out, t)
	}

	// flushCJK 把攒下的汉字串切成 2-gram 与 3-gram。
	flushCJK := func() {
		for n := 2; n <= 3; n++ {
			for i := 0; i+n <= len(cjk); i++ {
				add(string(cjk[i : i+n]))
			}
		}
		cjk = cjk[:0]
	}
	flushLatin := func() {
		if len(latin) > 0 {
			add(string(latin))
			latin = latin[:0]
		}
	}

	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			flushLatin()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			latin = append(latin, r)
		default:
			// 标点/空白是天然的分隔符。
			flushCJK()
			flushLatin()
		}
	}
	flushCJK()
	flushLatin()
	return out
}

// cjkStopGrams 是纯功能词的组合，作为词元没有区分度。
//
// 刻意只放虚词/指示词，不放"知道""觉得"这类带一点语义的词：
// 滤过头会把真有关联的内容也滤掉，而打捞的失败方向应当是**多捞**
// （捞回来的是整段上下文，由 LLM 自己判断有没有用，§5.1）。
var cjkStopGrams = map[string]bool{
	"什么": true, "时候": true, "一个": true, "这个": true, "那个": true,
	"可以": true, "就是": true, "还是": true, "因为": true, "所以": true,
	"但是": true, "如果": true, "现在": true, "自己": true, "已经": true,
	"这样": true, "那样": true, "起来": true, "出来": true, "过去": true,
	"然后": true, "之后": true, "之前": true, "一下": true, "有点": true,
	"一些": true, "这些": true, "那些": true, "不是": true, "不能": true,
	"不要": true, "怎么": true, "为了": true, "而且": true, "或者": true,
	"只是": true, "好像": true, "似乎": true, "其实": true, "反正": true,
	"于是": true, "不过": true, "的话": true, "一样": true, "一直": true,
	"一定": true, "而已": true, "之类": true, "来看": true,
}
