package pawn

import (
	"fmt"
	"sort"
	"strings"
)

type MergeOptions struct {
	Tolerance float64
	Sample    []Message
	// InitialBits 可缓存输入谓词在 Sample 上的命中位图，key 为 Filter.ID。
	InitialBits map[string][]uint64
	IDPrefix    string
}

// MergeFilters 把结构相近的直接输入过滤器做贪心合并。
//
// 不变量：每个输出 Filter 都覆盖其 SourceIDs 中所有输入 Filter，即任何被任一输入
// 命中的消息也一定被输出命中。合并允许额外命中，额外部分由下一级/门店本地精确表筛掉。
func MergeFilters(inputs []Filter, opt MergeOptions) []Filter {
	if len(inputs) == 0 {
		return nil
	}
	tol := opt.Tolerance
	if tol < 0 {
		tol = 0
	}
	if tol > 5 {
		tol = 5
	}

	groups := map[string][]int{}
	keys := make([]string, 0)
	for i := range inputs {
		k := shapeKey(inputs[i].Predicate)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], i)
	}
	sort.Strings(keys)

	// sortKey 里有 fmt 格式化；几十万输入时必须只计算一次。
	sortKeys := make([]string, len(inputs))
	for i := range inputs {
		sortKeys[i] = sortKey(&inputs[i])
	}

	var out []Filter
	for _, k := range keys {
		idxs := groups[k]
		sort.SliceStable(idxs, func(a, b int) bool {
			ka, kb := sortKeys[idxs[a]], sortKeys[idxs[b]]
			if ka != kb {
				return ka < kb
			}
			return inputs[idxs[a]].ID < inputs[idxs[b]].ID
		})

		var cur *cluster
		for _, idx := range idxs {
			in := inputs[idx]
			nb := newCluster(in, opt)
			if cur == nil {
				cur = nb
				continue
			}

			merged := mergeCluster(cur, nb, opt.Sample)
			baseCount := bitsCount(merged.base)
			extra := bitsCount(merged.predBits) - baseCount
			if extra < 0 {
				extra = 0
			}
			// 页面上的“允许超投比例”按样本消息总量计量：合并过滤器多放进的样本
			// 占整个观察样本流的比例。真实链路超投比另由全量仿真统计。
			overhead := 0.0
			if len(opt.Sample) > 0 {
				overhead = float64(extra) / float64(len(opt.Sample))
			}
			merged.filter.BaseMatches = baseCount
			merged.filter.EstimatedOverhead = overhead

			if overhead <= tol {
				cur = merged
			} else {
				out = append(out, cur.filter)
				cur = nb
			}
		}
		if cur != nil {
			out = append(out, cur.filter)
		}
	}

	// 稳定输出，便于页面和测试对比。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	prefix := opt.IDPrefix
	if prefix == "" {
		prefix = "MRG"
	}
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("%s-M%06d", prefix, i+1)
		}
		if out[i].MergedCount <= 0 {
			out[i].MergedCount = len(out[i].SourceIDs)
			if out[i].MergedCount == 0 {
				out[i].MergedCount = 1
			}
		}
	}
	return out
}

type cluster struct {
	filter   Filter
	base     []uint64 // 所有直接输入谓词命中样本的并集
	predBits []uint64 // 当前合并谓词命中的样本
}

func newCluster(f Filter, opt MergeOptions) *cluster {
	var bits []uint64
	if cached := opt.InitialBits[f.ID]; cached != nil {
		bits = append([]uint64(nil), cached...)
	} else {
		bits = predicateBits(f.Predicate, opt.Sample)
	}
	c := &cluster{
		filter:   f,
		base:     append([]uint64(nil), bits...),
		predBits: bits,
	}
	if c.filter.MergedCount <= 0 {
		c.filter.MergedCount = 1
	}
	return c
}

func mergeCluster(a, b *cluster, sample []Message) *cluster {
	f := generalizeFilters(a.filter, b.filter)
	base := orBits(a.base, b.base)
	predBits := predicateBits(f.Predicate, sample)

	// SourceIDs 只保存当前这张输入表中的直接记录 ID。若 cluster 已经合并过，
	// 它的 filter.SourceIDs 就是这些直接 ID；否则使用输入记录自身的 ID。
	sources := make([]string, 0, len(a.filter.SourceIDs)+len(b.filter.SourceIDs)+2)
	if len(a.filter.SourceIDs) > 0 {
		sources = append(sources, a.filter.SourceIDs...)
	} else if a.filter.ID != "" {
		sources = append(sources, a.filter.ID)
	}
	if len(b.filter.SourceIDs) > 0 {
		sources = append(sources, b.filter.SourceIDs...)
	} else if b.filter.ID != "" {
		sources = append(sources, b.filter.ID)
	}
	f.SourceIDs = uniqStrings(sources)

	f.MergedCount = a.filter.MergedCount + b.filter.MergedCount
	f.BaseMatches = bitsCount(base)
	f.FilterMatches = bitsCount(predBits)
	return &cluster{filter: f, base: base, predBits: predBits}
}

func generalizeFilters(a, b Filter) Filter {
	pa, pb := a.Predicate, b.Predicate
	ia := sortedClauseIndices(pa)
	ib := sortedClauseIndices(pb)
	clauses := make([]Clause, 0, len(ia))
	n := len(ia)
	if len(ib) < n {
		n = len(ib)
	}
	for k := 0; k < n; k++ {
		clauses = append(clauses, generalizeClause(pa.Clauses[ia[k]], pb.Clauses[ib[k]]))
	}
	return Filter{
		Predicate: Predicate{
			Clauses:        clauses,
			MissingAsMatch: true, // 覆盖任一方的 missing-match 语义；额外消息由下级精确过滤。
		},
	}
}

func shapeKey(p Predicate) string {
	idx := sortedClauseIndices(p)
	parts := make([]string, 0, len(idx)+1)
	for _, i := range idx {
		c := p.Clauses[i]
		family := string(c.Op)
		if !c.Numeric && (c.Op == OpEqual || c.Op == OpIn) {
			family = "eq"
		}
		if c.Numeric {
			family = "num"
		}
		part := c.Attr + ":" + family
		// 品类是最强的剪枝维度；不能把“名表”和“黄金”在门店合并阶段混成一个
		// 宽 IN，否则上级会对完全不相关的门店也转发。
		if c.Attr == AttrCategory {
			vals := append([]string{}, c.Values...)
			if c.Op == OpEqual {
				vals = append(vals, c.Str)
			}
			sort.Strings(vals)
			part += "=" + strings.Join(vals, "/")
		}
		parts = append(parts, part)
	}
	if p.MissingAsMatch {
		parts = append(parts, "missing=match")
	}
	return strings.Join(parts, "|")
}

func sortedClauseIndices(p Predicate) []int {
	idx := make([]int, len(p.Clauses))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		ca, cb := p.Clauses[idx[a]], p.Clauses[idx[b]]
		if ca.Attr != cb.Attr {
			return ca.Attr < cb.Attr
		}
		return shapeOp(ca) < shapeOp(cb)
	})
	return idx
}

func shapeOp(c Clause) string {
	if c.Numeric {
		return "num"
	}
	if c.Op == OpEqual || c.Op == OpIn {
		return "eq"
	}
	return string(c.Op)
}

func similarityLess(a, b *Filter) bool {
	ka := sortKey(a)
	kb := sortKey(b)
	if ka != kb {
		return ka < kb
	}
	return a.ID < b.ID
}

func sortKey(f *Filter) string {
	for _, c := range f.Clauses {
		if c.Numeric && c.HasLow {
			return fmt.Sprintf("%s:%012.2f", c.Attr, c.Low)
		}
	}
	for _, c := range f.Clauses {
		if !c.Numeric {
			return c.Attr + ":" + c.Str + strings.Join(c.Values, ",")
		}
	}
	return f.ID
}

func generalizeClause(a, b Clause) Clause {
	if a.Numeric || b.Numeric {
		return generalizeNumeric(a, b)
	}
	switch shapeOp(a) {
	case "eq":
		vals := make([]string, 0, len(a.Values)+len(b.Values)+2)
		if a.Op == OpEqual {
			vals = append(vals, a.Str)
		} else {
			vals = append(vals, a.Values...)
		}
		if b.Op == OpEqual {
			vals = append(vals, b.Str)
		} else {
			vals = append(vals, b.Values...)
		}
		vals = uniqStrings(vals)
		if len(vals) == 1 {
			return Clause{Attr: a.Attr, Op: OpEqual, Str: vals[0]}
		}
		return Clause{Attr: a.Attr, Op: OpIn, Values: vals}
	case "prefix":
		pre := longestCommonPrefix(a.Str, b.Str)
		return Clause{Attr: a.Attr, Op: OpPrefix, Str: pre}
	case "contains":
		sub := longestCommonSubstring(a.Str, b.Str)
		return Clause{Attr: a.Attr, Op: OpContains, Str: sub}
	default:
		// 未知字符串操作无法安全泛化时，空条件（例如空前缀/包含）是合法覆盖。
		return Clause{Attr: a.Attr, Op: OpPrefix, Str: ""}
	}
}

func generalizeNumeric(a, b Clause) Clause {
	// 统一抽取为下界和上界。生成器通常每个属性只有一个数值条件；
	// 对 equal/单侧/between 做安全包络。
	loA, hiA, hasLoA, hasHiA, loIncA, hiIncA := numericBounds(a)
	loB, hiB, hasLoB, hasHiB, loIncB, hiIncB := numericBounds(b)

	lo, hasLo, loInc := 0.0, false, false
	if hasLoA && hasLoB {
		// 只有两个输入都有下界时才能保留下界；任一方左侧无界，合并结果也必须左侧无界。
		hasLo = true
		if loA < loB {
			lo, loInc = loA, loIncA
		} else if loB < loA {
			lo, loInc = loB, loIncB
		} else {
			lo, loInc = loA, loIncA || loIncB
		}
	}

	hi, hasHi, hiInc := 0.0, false, false
	if hasHiA && hasHiB {
		// 同理，只有两个输入都有上界时才能保留上界。
		hasHi = true
		if hiA > hiB {
			hi, hiInc = hiA, hiIncA
		} else if hiB > hiA {
			hi, hiInc = hiB, hiIncB
		} else {
			hi, hiInc = hiA, hiIncA || hiIncB
		}
	}

	if hasLo && hasHi && lo == hi && loInc && hiInc {
		return Clause{Attr: a.Attr, Op: OpEqual, Numeric: true, Low: lo, HasLow: true}
	}
	return Clause{
		Attr: a.Attr, Op: OpBetween, Numeric: true,
		Low: lo, High: hi, HasLow: hasLo, HasHigh: hasHi,
		LowInclusive: loInc, HighInclusive: hiInc,
	}
}

func numericBounds(c Clause) (lo, hi float64, hasLo, hasHi, loInc, hiInc bool) {
	switch c.Op {
	case OpEqual:
		return c.Low, c.Low, c.HasLow, c.HasLow, true, true
	case OpGreater:
		return c.Low, 0, c.HasLow, false, false, false
	case OpGTE:
		return c.Low, 0, c.HasLow, false, true, false
	case OpLess:
		return 0, c.High, false, c.HasHigh, false, false
	case OpLTE:
		return 0, c.High, false, c.HasHigh, false, true
	case OpBetween:
		return c.Low, c.High, c.HasLow, c.HasHigh, c.LowInclusive, c.HighInclusive
	default:
		// 无法识别时不加边界，相当于通配，仍保证覆盖。
		return 0, 0, false, false, false, false
	}
}

func longestCommonPrefix(a, b string) string {
	ra, rb := []rune(a), []rune(b)
	n := len(ra)
	if len(rb) < n {
		n = len(rb)
	}
	i := 0
	for i < n && ra[i] == rb[i] {
		i++
	}
	return string(ra[:i])
}

func longestCommonSubstring(a, b string) string {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return ""
	}
	prev := make([]int, len(rb)+1)
	bestLen, bestEnd := 0, 0
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > bestLen {
					bestLen = cur[j]
					bestEnd = i
				}
			}
		}
		prev = cur
	}
	if bestLen == 0 {
		return ""
	}
	return string(ra[bestEnd-bestLen : bestEnd])
}

func predicateBits(p Predicate, sample []Message) []uint64 {
	bits := make([]uint64, (len(sample)+63)/64)
	for i := range sample {
		if p.Matches(&sample[i]) {
			bits[i/64] |= 1 << uint(i%64)
		}
	}
	return bits
}

func orBits(a, b []uint64) []uint64 {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	out := make([]uint64, n)
	copy(out, a)
	for i := range b {
		out[i] |= b[i]
	}
	return out
}

func bitsCount(bits []uint64) int {
	n := 0
	for _, x := range bits {
		n += PopCount64(x)
	}
	return n
}

func uniqStrings(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if x == "" {
			// 空字符串可能是“空前缀”等有效泛化值，但在 SourceIDs 中不应出现。
			// 调用 uniqStrings 处理条件值时空值也可保留；为了两个用途分开，这里保留空值。
		}
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}
