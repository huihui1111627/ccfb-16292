package pawn

import (
	"fmt"
	"math/rand"
	"strings"
)

type PlannedSub struct {
	Sub       Subscription
	AddSeq    int
	DeleteSeq int // MessageCount+1 表示不删除
}

type SubChange struct {
	Seq int
	Add bool
	Sub Subscription
}

type Generator struct {
	cfg Config
	r   *rand.Rand
}

func NewGenerator(cfg Config) *Generator {
	return &Generator{cfg: cfg, r: rand.New(rand.NewSource(cfg.Seed))}
}

func (g *Generator) Config() Config { return g.cfg }

func StoreID(i int) string { return fmt.Sprintf("S%02d", i+1) }
func RegionID(i int) string {
	if i < 8 {
		return fmt.Sprintf("R%d", i+1)
	}
	return fmt.Sprintf("RX%d", i-7)
}

// InitialTopology 生成确定性的总部-区域-门店树。
func InitialTopology(cfg Config) (root string, regions []string, stores []string, parent map[string]string) {
	root = "HQ"
	regions = make([]string, cfg.RegionCount)
	parent = map[string]string{"HQ": ""}
	for i := 0; i < cfg.RegionCount; i++ {
		regions[i] = fmt.Sprintf("R%d", i+1)
		parent[regions[i]] = root
	}
	stores = make([]string, cfg.StoreCount)
	for i := 0; i < cfg.StoreCount; i++ {
		stores[i] = StoreID(i)
		r := regions[i%cfg.RegionCount]
		parent[stores[i]] = r
	}
	return
}

func (g *Generator) PlannedSubs() []PlannedSub {
	cfg := g.cfg
	total := cfg.InitialPerStore*cfg.StoreCount + cfg.ExtraSubs
	initialTotal := cfg.InitialPerStore * cfg.StoreCount
	out := make([]PlannedSub, total)

	// 动态增删按固定批次边界生效。这样既能让增删贯穿整个消息流，又避免每条订阅
	// 变更都把 HQ 几万条合并表全量重算一次；oracle 使用同一个 AddSeq/DeleteSeq。
	batchCount := 24
	if cfg.MessageCount < batchCount {
		batchCount = max(1, cfg.MessageCount)
	}
	step := max(1, cfg.MessageCount/batchCount)

	// 随机选择一部分初始订阅删除。
	deleteAt := make(map[int]int, cfg.DeleteCount)
	if cfg.DeleteCount > 0 && initialTotal > 0 {
		idx := g.r.Perm(initialTotal)
		for k := 0; k < cfg.DeleteCount && k < len(idx); k++ {
			rawSeq := 20_000 + g.r.Intn(max(1, cfg.MessageCount-40_000))
			batch := rawSeq / step
			if batch < 1 {
				batch = 1
			}
			seq := batch * step
			if seq >= cfg.MessageCount {
				seq = cfg.MessageCount - 1
			}
			deleteAt[idx[k]] = seq
		}
	}

	for i := 0; i < total; i++ {
		storeIdx := i % cfg.StoreCount
		store := StoreID(storeIdx)
		sub := g.makeSubscription(i, store, storeIdx)
		p := PlannedSub{Sub: sub, AddSeq: 0, DeleteSeq: cfg.MessageCount + 1}
		if i >= initialTotal {
			j := i - initialTotal
			batch := 2 + (j % max(1, batchCount-3))
			p.AddSeq = batch * step
			if p.AddSeq >= cfg.MessageCount {
				p.AddSeq = cfg.MessageCount - 1
			}
		}
		if seq, ok := deleteAt[i]; ok {
			p.DeleteSeq = seq
		}
		out[i] = p
	}
	return out
}

func (g *Generator) Changes(planned []PlannedSub) map[int][]SubChange {
	m := make(map[int][]SubChange)
	for _, p := range planned {
		if p.AddSeq > 0 {
			m[p.AddSeq] = append(m[p.AddSeq], SubChange{Seq: p.AddSeq, Add: true, Sub: p.Sub})
		}
		if p.DeleteSeq <= g.cfg.MessageCount {
			m[p.DeleteSeq] = append(m[p.DeleteSeq], SubChange{Seq: p.DeleteSeq, Add: false, Sub: p.Sub})
		}
	}
	return m
}

func (g *Generator) makeSubscription(i int, store string, storeIdx int) Subscription {
	r := g.r
	id := fmt.Sprintf("SUB-%07d", i+1)
	missingMatch := r.Intn(100) < 14
	p := Predicate{MissingAsMatch: missingMatch}

	// 每家门店有明确主营方向。除两个混合画像外，不撒跨品类探索订阅，保证上级
	// 代理能按品类把消息挡在不相关门店之外。
	x := r.Intn(100)
	profile := storeIdx % 8
	switch profile {
	case 0, 5: // 黄金/金饰
		x = r.Intn(34)
	case 1, 6: // 名表
		x = 34 + r.Intn(26)
	case 2: // 名牌包/珠宝
		x = 60 + r.Intn(16)
	case 3: // 古董/玉器
		x = 76 + r.Intn(15)
	case 4, 7: // 混合高价值：表/珠宝/古董
		bucket := r.Intn(3)
		if bucket == 0 {
			x = 34 + r.Intn(26)
		} else if bucket == 1 {
			x = 60 + r.Intn(16)
		} else {
			x = 76 + r.Intn(15)
		}
	}

	switch {
	case x < 34:
		// 黄金/克重场景。
		p.Clauses = append(p.Clauses, g.categoryClause())
		if r.Intn(100) < 85 {
			p.Clauses = append(p.Clauses, g.weightClause())
		}
		if r.Intn(100) < 48 {
			p.Clauses = append(p.Clauses, g.appraisalClause())
		}
		if r.Intn(100) < 12 {
			p.Clauses = append(p.Clauses, g.stringClause(AttrBrand, pick(r, []string{"ChowTaiFook", "LaoFengXiang", "Cartier", "Tiffany"})))
		}

	case x < 60:
		// 名表估价区间。
		p.Clauses = append(p.Clauses, Clause{Attr: AttrCategory, Op: OpEqual, Str: "名表"})
		if r.Intn(100) < 70 {
			b := pick(r, brands)
			if r.Intn(100) < 25 {
				p.Clauses = append(p.Clauses, Clause{Attr: AttrBrand, Op: OpEqual, Str: b})
			} else {
				p.Clauses = append(p.Clauses, g.stringClause(AttrBrand, b))
			}
		}
		if r.Intn(100) < 38 {
			p.Clauses = append(p.Clauses, g.stringClause(AttrWatchModel, pick(r, watchModels)))
		}
		if r.Intn(100) < 88 {
			p.Clauses = append(p.Clauses, g.appraisalClause())
		}

	case x < 76:
		// 名牌包/珠宝。
		if r.Intn(2) == 0 {
			p.Clauses = append(p.Clauses, Clause{Attr: AttrCategory, Op: OpEqual, Str: pick(r, []string{"名牌包", "珠宝", "钻石"})})
		} else {
			p.Clauses = append(p.Clauses, g.inClause(AttrCategory, []string{"名牌包", "珠宝", "钻石"}))
		}
		if r.Intn(100) < 78 {
			p.Clauses = append(p.Clauses, g.stringClause(AttrBrand, pick(r, brands)))
		}
		if r.Intn(100) < 62 {
			p.Clauses = append(p.Clauses, g.appraisalClause())
		}
		if r.Intn(100) < 25 {
			p.Clauses = append(p.Clauses, Clause{Attr: AttrStyle, Op: OpEqual, Str: pick(r, styles)})
		}

	case x < 91:
		// 古董/玉器。
		if r.Intn(2) == 0 {
			p.Clauses = append(p.Clauses, Clause{Attr: AttrCategory, Op: OpEqual, Str: pick(r, []string{"古董", "玉器", "翡翠"})})
		} else {
			p.Clauses = append(p.Clauses, g.inClause(AttrCategory, []string{"古董", "玉器", "翡翠", "金饰"}))
		}
		if r.Intn(100) < 58 {
			p.Clauses = append(p.Clauses, g.stringClause(AttrEra, pick(r, eras)))
		}
		if r.Intn(100) < 72 {
			p.Clauses = append(p.Clauses, g.appraisalClause())
		}

	default:
		// 较宽的品类订阅，用于覆盖高扇出路径。
		if r.Intn(2) == 0 {
			p.Clauses = append(p.Clauses, Clause{Attr: AttrCategory, Op: OpEqual, Str: pick(r, allCategories)})
		} else {
			p.Clauses = append(p.Clauses, g.inClause(AttrCategory, allCategories))
		}
		if r.Intn(100) < 22 {
			p.Clauses = append(p.Clauses, g.appraisalClause())
		}
	}

	return Subscription{ID: id, StoreID: store, Predicate: p}
}

func (g *Generator) categoryClause() Clause {
	r := g.r
	switch r.Intn(100) {
	case 0, 1, 2:
		return g.inClause(AttrCategory, append([]string{}, goldCategories...))
	case 3, 4:
		return g.inClause(AttrCategory, []string{"黄金", "金饰", "古董"})
	default:
		return Clause{Attr: AttrCategory, Op: OpEqual, Str: pick(r, goldCategories)}
	}
}

func (g *Generator) inClause(attr string, choices []string) Clause {
	r := g.r
	n := 2 + r.Intn(min(3, len(choices)-1))
	if n > len(choices) {
		n = len(choices)
	}
	shuffled := append([]string{}, choices...)
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	return Clause{Attr: attr, Op: OpIn, Values: shuffled[:n]}
}

func (g *Generator) stringClause(attr, value string) Clause {
	r := g.r
	runes := []rune(value)
	if len(runes) == 0 {
		return Clause{Attr: attr, Op: OpEqual, Str: value}
	}
	switch r.Intn(3) {
	case 0:
		return Clause{Attr: attr, Op: OpEqual, Str: value}
	case 1:
		n := 1 + r.Intn(len(runes))
		pre := string(runes[:n])
		return Clause{Attr: attr, Op: OpPrefix, Str: pre}
	default:
		if len(runes) == 1 {
			return Clause{Attr: attr, Op: OpContains, Str: value}
		}
		start := r.Intn(len(runes) - 1)
		maxLen := min(3, len(runes)-start)
		n := 1 + r.Intn(maxLen)
		return Clause{Attr: attr, Op: OpContains, Str: string(runes[start : start+n])}
	}
}

func (g *Generator) weightClause() Clause {
	r := g.r
	threshold := weightBounds[r.Intn(len(weightBounds))]
	switch r.Intn(100) {
	case 0, 1, 2, 3:
		// 明确覆盖 >20 这个典型条件。
		return Clause{Attr: AttrGoldWeight, Op: OpGreater, Numeric: true, Low: 20, HasLow: true, LowInclusive: false}
	case 4, 5, 6:
		return Clause{Attr: AttrGoldWeight, Op: OpGTE, Numeric: true, Low: 20, HasLow: true, LowInclusive: true}
	}
	if r.Intn(100) < 55 {
		if r.Intn(2) == 0 {
			return Clause{Attr: AttrGoldWeight, Op: OpGreater, Numeric: true, Low: threshold, HasLow: true, LowInclusive: false}
		}
		return Clause{Attr: AttrGoldWeight, Op: OpGTE, Numeric: true, Low: threshold, HasLow: true, LowInclusive: true}
	}
	lo := weightBounds[r.Intn(len(weightBounds)-1)]
	hi := lo
	for hi <= lo {
		hi = weightBounds[r.Intn(len(weightBounds))]
	}
	return rangeClause(AttrGoldWeight, lo, hi, r)
}

func (g *Generator) appraisalClause() Clause {
	r := g.r
	lo := appraisalBounds[r.Intn(len(appraisalBounds)-1)]
	hi := lo
	for hi <= lo {
		hi = appraisalBounds[r.Intn(len(appraisalBounds))]
	}
	// 有时生成单侧区间。
	switch r.Intn(10) {
	case 0:
		return Clause{Attr: AttrAppraisal, Op: OpGTE, Numeric: true, Low: lo, HasLow: true, LowInclusive: r.Intn(2) == 0}
	case 1:
		return Clause{Attr: AttrAppraisal, Op: OpLTE, Numeric: true, High: hi, HasHigh: true, HighInclusive: r.Intn(2) == 0}
	default:
		return rangeClause(AttrAppraisal, lo, hi, r)
	}
}

func rangeClause(attr string, lo, hi float64, r *rand.Rand) Clause {
	lowInc := r.Intn(2) == 0
	highInc := r.Intn(2) == 0
	return Clause{
		Attr: attr, Op: OpBetween, Numeric: true,
		Low: lo, High: hi, HasLow: true, HasHigh: true,
		LowInclusive: lowInc, HighInclusive: highInc,
	}
}

func (g *Generator) Message(seq int) Message {
	r := g.r
	cfg := g.cfg
	id := fmt.Sprintf("MSG-%07d", seq+1)
	origin := StoreID(r.Intn(cfg.StoreCount))
	attrs := map[string]Values{}
	display := []AttrView{}

	addStr := func(attr, label string, vals []string) {
		attrs[attr] = Values{Str: vals}
		display = append(display, AttrView{Name: attr, Label: label, Value: strings.Join(vals, " / ")})
	}
	addNum := func(attr, label string, v float64, suffix string) {
		attrs[attr] = Values{Num: []float64{v}}
		display = append(display, AttrView{Name: attr, Label: label, Value: formatNumber(v) + suffix})
	}

	archetype := r.Intn(100)
	var cats []string
	switch {
	case archetype < 35:
		primary := pick(r, goldCategories)
		cats = []string{primary}
		if r.Intn(100) < 72 && !contains(cats, "金饰") {
			cats = append(cats, "金饰")
		}
		if r.Intn(100) < 10 {
			cats = append(cats, "古董")
		}
		addStr(AttrCategory, AttrLabels[AttrCategory], uniq(cats))
		if r.Intn(100) < 88 {
			w := g.numericNearBounds(weightBounds, 1, 220)
			addNum(AttrGoldWeight, AttrLabels[AttrGoldWeight], w, "g")
		}
		if r.Intn(100) < 90 {
			addNum(AttrAppraisal, AttrLabels[AttrAppraisal], g.appraisalValue(), "元")
		}
		if r.Intn(100) < 24 {
			b := pick(r, []string{"ChowTaiFook", "LaoFengXiang", "Cartier", "Tiffany"})
			addStr(AttrBrand, AttrLabels[AttrBrand], []string{b})
		}

	case archetype < 60:
		cats = []string{"名表"}
		addStr(AttrCategory, AttrLabels[AttrCategory], cats)
		if r.Intn(100) < 76 {
			addStr(AttrBrand, AttrLabels[AttrBrand], []string{pick(r, brands)})
		}
		if r.Intn(100) < 68 {
			addStr(AttrWatchModel, AttrLabels[AttrWatchModel], []string{pick(r, watchModels)})
		}
		if r.Intn(100) < 94 {
			addNum(AttrAppraisal, AttrLabels[AttrAppraisal], g.appraisalValue(), "元")
		}

	case archetype < 76:
		c := pick(r, []string{"名牌包", "珠宝", "钻石"})
		cats = []string{c}
		if r.Intn(100) < 25 {
			cats = append(cats, "珠宝")
		}
		addStr(AttrCategory, AttrLabels[AttrCategory], uniq(cats))
		if r.Intn(100) < 82 {
			addStr(AttrBrand, AttrLabels[AttrBrand], []string{pick(r, brands)})
		}
		if r.Intn(100) < 35 {
			addStr(AttrStyle, AttrLabels[AttrStyle], []string{pick(r, styles)})
		}
		if r.Intn(100) < 70 {
			addNum(AttrAppraisal, AttrLabels[AttrAppraisal], g.appraisalValue(), "元")
		}

	case archetype < 91:
		c := pick(r, []string{"古董", "玉器", "翡翠"})
		cats = []string{c}
		if r.Intn(100) < 35 {
			cats = append(cats, pick(r, []string{"金饰", "玉器", "翡翠"}))
		}
		addStr(AttrCategory, AttrLabels[AttrCategory], uniq(cats))
		if r.Intn(100) < 72 {
			addStr(AttrEra, AttrLabels[AttrEra], []string{pick(r, eras)})
		}
		if r.Intn(100) < 78 {
			addNum(AttrAppraisal, AttrLabels[AttrAppraisal], g.appraisalValue(), "元")
		}

	default:
		cats = []string{pick(r, allCategories)}
		if r.Intn(100) < 35 {
			cats = append(cats, pick(r, allCategories))
		}
		addStr(AttrCategory, AttrLabels[AttrCategory], uniq(cats))
		if r.Intn(100) < 55 {
			addNum(AttrAppraisal, AttrLabels[AttrAppraisal], g.appraisalValue(), "元")
		}
		if r.Intn(100) < 35 {
			addStr(AttrBrand, AttrLabels[AttrBrand], []string{pick(r, brands)})
		}
	}

	return Message{Seq: seq, ID: id, OriginID: origin, Attrs: attrs, Display: display}
}

func (g *Generator) SampleMessages(n int) []Message {
	sampleR := rand.New(rand.NewSource(g.cfg.Seed + 777))
	old := g.r
	g.r = sampleR
	defer func() { g.r = old }()
	out := make([]Message, n)
	for i := 0; i < n; i++ {
		out[i] = g.Message(10_000_000 + i)
	}
	return out
}

func (g *Generator) numericNearBounds(bounds []float64, minV, maxV float64) float64 {
	r := g.r
	if r.Intn(100) < 38 {
		return bounds[r.Intn(len(bounds))]
	}
	v := minV + r.Float64()*(maxV-minV)
	return float64(int(v*10)) / 10
}

func (g *Generator) appraisalValue() float64 {
	r := g.r
	if r.Intn(100) < 42 {
		return appraisalBounds[r.Intn(len(appraisalBounds))]
	}
	// 偏对数分布，避免全部挤在低价区间。
	v := 500 * pow(4000, r.Float64())
	if v > 2_000_000 {
		v = 2_000_000
	}
	return float64(int(v))
}

func pow(base, exp float64) float64 {
	// 避免在文件顶部再引 math 造成名称混杂；这里直接用 math.Pow。
	return mathPow(base, exp)
}

func pick[T any](r *rand.Rand, xs []T) T {
	return xs[r.Intn(len(xs))]
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func uniq(xs []string) []string {
	seen := map[string]struct{}{}
	out := xs[:0]
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
