package pawn

import (
	"hash/maphash"
	"math"
	"strings"
	"sync"
)

// Matcher 是只读的候选索引。
//
// 每条 Predicate 选择一个最选择性的 Clause 作为 anchor，只在该 Clause 的索引里登记一次。
// 查询时从消息命中的各类 anchor 索引收集候选，再做完整的直接判断。这样既不会扫描全表，
// 也不会因为索引近似而漏掉订阅：anchor 命中是整条订阅命中的必要条件。
type Matcher struct {
	preds   []Predicate
	anchors []int

	all []int32 // 没有条件的全匹配谓词

	exact    map[string]map[string][]int32
	prefix   map[string]map[string][]int32
	contains map[string]map[string][]int32
	wildcard map[string][]int32
	numbers  map[string]*intervalTree

	// 品类几乎总是合取条件之一。为所有 eq/in 品类谓词额外建位图，在 anchor
	// 候选上先做一次廉价交集，避免大量跨品类候选进入完整条件判断。
	categoryBits    map[string][]uint64
	noCategory      []uint64
	missingCategory []uint64

	// MissingAsMatch=true 的谓词按 anchor 属性登记；消息缺少该属性时也要成为候选。
	missing map[string][]int32

	pool sync.Pool
}

type matchState struct {
	seen    []uint32
	stamp   uint32
	catMask []uint64
}

func NewMatcher(preds []Predicate) *Matcher {
	copied := make([]Predicate, len(preds))
	copy(copied, preds)
	m := &Matcher{
		preds:           copied,
		anchors:         make([]int, len(preds)),
		exact:           make(map[string]map[string][]int32),
		prefix:          make(map[string]map[string][]int32),
		contains:        make(map[string]map[string][]int32),
		wildcard:        make(map[string][]int32),
		numbers:         make(map[string]*intervalTree),
		categoryBits:    make(map[string][]uint64),
		noCategory:      make([]uint64, (len(preds)+63)/64),
		missingCategory: make([]uint64, (len(preds)+63)/64),
		missing:         make(map[string][]int32),
	}
	m.pool.New = func() any {
		return &matchState{seen: make([]uint32, len(m.preds)), catMask: make([]uint64, (len(m.preds)+63)/64)}
	}

	for h := range m.preds {
		p := &m.preds[h]
		a := chooseAnchor(p)
		m.anchors[h] = a
		id := int32(h)
		if a < 0 {
			m.all = append(m.all, id)
			// 空条件是 tautology，也必须进入品类门控的 noCategory 位图。
			m.indexCategoryBit(id, p)
			continue
		}
		c := &p.Clauses[a]
		if p.MissingAsMatch {
			m.missing[c.Attr] = append(m.missing[c.Attr], id)
		}
		m.indexAnchor(id, c)
		m.indexCategoryBit(id, &m.preds[h])
	}
	return m
}

func setBit(bits []uint64, h int32) {
	bits[uint(h)/64] |= 1 << (uint(h) % 64)
}

func hasBit(bits []uint64, h int32) bool {
	return bits[uint(h)/64]&(1<<(uint(h)%64)) != 0
}

func orBitsInto(dst, src []uint64) {
	n := len(dst)
	if len(src) < n {
		n = len(src)
	}
	for i := 0; i < n; i++ {
		dst[i] |= src[i]
	}
}

func (m *Matcher) indexCategoryBit(h int32, p *Predicate) {
	if p.MissingAsMatch {
		setBit(m.missingCategory, h)
	}
	for i := range p.Clauses {
		c := &p.Clauses[i]
		if c.Attr != AttrCategory || c.Numeric {
			continue
		}
		switch c.Op {
		case OpEqual:
			bits := m.categoryBits[c.Str]
			if bits == nil {
				bits = make([]uint64, (len(m.preds)+63)/64)
				m.categoryBits[c.Str] = bits
			}
			setBit(bits, h)
			return
		case OpIn:
			for _, v := range c.Values {
				bits := m.categoryBits[v]
				if bits == nil {
					bits = make([]uint64, (len(m.preds)+63)/64)
					m.categoryBits[v] = bits
				}
				setBit(bits, h)
			}
			return
		}
		// prefix/contains 品类条件交给完整判断，不做位图预筛。
		break
	}
	setBit(m.noCategory, h)
	if p.MissingAsMatch {
		setBit(m.missingCategory, h)
	}
}

func (m *Matcher) Len() int { return len(m.preds) }

func chooseAnchor(p *Predicate) int {
	best := -1
	bestScore := math.MaxInt
	for i := range p.Clauses {
		s := anchorScore(&p.Clauses[i])
		if s < bestScore {
			bestScore = s
			best = i
		}
	}
	return best
}

func anchorScore(c *Clause) int {
	if c.Numeric {
		switch c.Op {
		case OpEqual:
			return 12
		case OpBetween:
			width := 0.0
			if c.HasLow && c.HasHigh && !math.IsInf(c.Low, 1) && !math.IsInf(c.High, -1) {
				width = c.High - c.Low
			}
			// 越窄越优先；不同属性的绝对量纲不同，生成器会把区间控制得较窄。
			if width <= 0 {
				return 18
			}
			return 28
		default:
			return 55
		}
	}

	selectiveAttr := 0
	switch c.Attr {
	case AttrBrand, AttrWatchModel, AttrEra, AttrStyle:
		selectiveAttr = -8
	case AttrCategory:
		return 90
	}
	switch c.Op {
	case OpEqual:
		return 20 + selectiveAttr
	case OpIn:
		return 24 + selectiveAttr + len(c.Values)/4
	case OpPrefix:
		return 35 - len([]rune(c.Str))*2 + selectiveAttr
	case OpContains:
		return 45 - len([]rune(c.Str))*2 + selectiveAttr
	default:
		return 80
	}
}

func (m *Matcher) indexAnchor(id int32, c *Clause) {
	if c.Numeric {
		t := m.numbers[c.Attr]
		if t == nil {
			t = newIntervalTree()
			m.numbers[c.Attr] = t
		}
		iv := clauseInterval(c)
		t.Insert(id, iv)
		return
	}

	switch c.Op {
	case OpEqual:
		m.addExact(c.Attr, c.Str, id)
	case OpIn:
		for _, v := range c.Values {
			m.addExact(c.Attr, v, id)
		}
	case OpPrefix:
		if c.Str == "" {
			m.wildcard[c.Attr] = append(m.wildcard[c.Attr], id)
			return
		}
		attrMap := m.prefix[c.Attr]
		if attrMap == nil {
			attrMap = make(map[string][]int32)
			m.prefix[c.Attr] = attrMap
		}
		attrMap[c.Str] = append(attrMap[c.Str], id)
	case OpContains:
		tok := containsToken(c.Str)
		if tok == "" {
			m.wildcard[c.Attr] = append(m.wildcard[c.Attr], id)
			return
		}
		attrMap := m.contains[c.Attr]
		if attrMap == nil {
			attrMap = make(map[string][]int32)
			m.contains[c.Attr] = attrMap
		}
		attrMap[tok] = append(attrMap[tok], id)
	}
}

func (m *Matcher) addExact(attr, val string, id int32) {
	attrMap := m.exact[attr]
	if attrMap == nil {
		attrMap = make(map[string][]int32)
		m.exact[attr] = attrMap
	}
	attrMap[val] = append(attrMap[val], id)
}

func clauseInterval(c *Clause) interval {
	iv := interval{low: math.Inf(-1), high: math.Inf(1), lowInc: true, highInc: true}
	switch c.Op {
	case OpEqual:
		if c.HasLow {
			iv.low, iv.high = c.Low, c.Low
			iv.lowInc, iv.highInc = true, true
		}
	case OpGreater:
		iv.low, iv.lowInc = c.Low, false
	case OpGTE:
		iv.low, iv.lowInc = c.Low, true
	case OpLess:
		iv.high, iv.highInc = c.High, false
	case OpLTE:
		iv.high, iv.highInc = c.High, true
	case OpBetween:
		if c.HasLow {
			iv.low, iv.lowInc = c.Low, c.LowInclusive
		}
		if c.HasHigh {
			iv.high, iv.highInc = c.High, c.HighInclusive
		}
	}
	return iv
}

// Match 返回所有完整条件均满足的谓词 handle。
func (m *Matcher) Match(msg *Message) []int32 {
	dst := make([]int32, 0, 16)
	return m.AppendMatch(msg, dst)
}

// AppendMatch 使用调用方缓冲和 Matcher 内的池化去重状态。
func (m *Matcher) AppendMatch(msg *Message, dst []int32) []int32 {
	if len(m.preds) == 0 {
		return dst
	}
	st := m.pool.Get().(*matchState)
	if len(st.seen) < len(m.preds) {
		st.seen = make([]uint32, len(m.preds))
	}
	st.stamp++
	if st.stamp == 0 {
		for i := range st.seen {
			st.seen[i] = 0
		}
		st.stamp = 1
	}
	stamp := st.stamp
	seen := st.seen
	words := (len(m.preds) + 63) / 64
	if len(st.catMask) < words {
		st.catMask = make([]uint64, words)
	}
	catMask := st.catMask[:words]
	for i := range catMask {
		catMask[i] = m.noCategory[i]
	}
	if catVals, ok := msg.StringValues(AttrCategory); ok {
		for _, v := range catVals {
			if bits := m.categoryBits[v]; bits != nil {
				orBitsInto(catMask, bits)
			}
		}
	} else {
		orBitsInto(catMask, m.missingCategory)
	}

	add := func(h int32) {
		if h < 0 || int(h) >= len(m.preds) || seen[h] == stamp {
			return
		}
		if !hasBit(catMask, h) {
			return
		}
		seen[h] = stamp
		if m.preds[h].Matches(msg) {
			dst = append(dst, h)
		}
	}

	for _, h := range m.all {
		add(h)
	}

	// 消息中出现的字符串值驱动 exact/prefix/contains 索引。
	for attr, vals := range msg.Attrs {
		if len(vals.Str) > 0 {
			if byVal := m.exact[attr]; byVal != nil {
				for _, v := range vals.Str {
					for _, h := range byVal[v] {
						add(h)
					}
				}
			}
			if byPrefix := m.prefix[attr]; byPrefix != nil {
				for _, v := range vals.Str {
					for _, pre := range allRunePrefixes(v) {
						for _, h := range byPrefix[pre] {
							add(h)
						}
					}
				}
			}
			if byContains := m.contains[attr]; byContains != nil {
				for _, v := range vals.Str {
					for _, tok := range ngramTokens(v) {
						for _, h := range byContains[tok] {
							add(h)
						}
					}
				}
			}
			// 空前缀/空包含是“该字符串属性存在即通过”的通配 anchor。
			for _, h := range m.wildcard[attr] {
				add(h)
			}
		}

		if len(vals.Num) > 0 {
			if tree := m.numbers[attr]; tree != nil {
				for _, x := range vals.Num {
					tree.Query(x, add)
				}
			}
		}
	}

	// MissingAsMatch 的谓词：若 anchor 属性按“该 Clause 的类型”缺失，也要成为候选。
	// 不能按属性名全局判断类型：同一属性理论上可能在不同谓词中承载不同类型。
	for attr, hs := range m.missing {
		v, ok := msg.Attrs[attr]
		for _, h := range hs {
			idx := m.anchors[h]
			present := false
			if ok && idx >= 0 && idx < len(m.preds[h].Clauses) {
				c := &m.preds[h].Clauses[idx]
				if c.Numeric {
					present = len(v.Num) > 0
				} else {
					present = len(v.Str) > 0
				}
			}
			if !present {
				add(h)
			}
		}
	}

	m.pool.Put(st)
	return dst
}

func allRunePrefixes(s string) []string {
	r := []rune(s)
	out := make([]string, 0, len(r))
	for i := 1; i <= len(r); i++ {
		out = append(out, string(r[:i]))
	}
	return out
}

func containsToken(s string) string {
	r := []rune(s)
	switch {
	case len(r) >= 3:
		return string(r[:3])
	case len(r) == 2:
		return string(r[:2])
	case len(r) == 1:
		return string(r[:1])
	default:
		return s
	}
}

func ngramTokens(s string) []string {
	r := []rune(s)
	seen := make(map[string]struct{}, len(r)*3)
	out := make([]string, 0, len(r)*3)
	add := func(t string) {
		if t == "" {
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for i := range r {
		add(string(r[i : i+1]))
		if i+2 <= len(r) {
			add(string(r[i : i+2]))
		}
		if i+3 <= len(r) {
			add(string(r[i : i+3]))
		}
	}
	return out
}

// 用 maphash 生成稳定的 treap 优先级；不依赖全局随机源。
var prioritySeed = maphash.MakeSeed()

func stablePriority(id int32, salt string) uint64 {
	var h maphash.Hash
	h.SetSeed(prioritySeed)
	_, _ = h.WriteString(salt)
	var b [4]byte
	b[0] = byte(id)
	b[1] = byte(id >> 8)
	b[2] = byte(id >> 16)
	b[3] = byte(id >> 24)
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// 避免未使用 strings 包（后续规范化可能使用）。
var _ = strings.TrimSpace
