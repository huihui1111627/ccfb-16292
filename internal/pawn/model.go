package pawn

import (
	"fmt"
	"strings"
)

// Op 是订阅条件支持的操作符。
type Op string

const (
	OpEqual    Op = "eq"
	OpIn       Op = "in"
	OpPrefix   Op = "prefix"
	OpContains Op = "contains"
	OpGreater  Op = "gt"
	OpGTE      Op = "gte"
	OpLess     Op = "lt"
	OpLTE      Op = "lte"
	OpBetween  Op = "between"
)

// Clause 是一个属性上的条件。
//
// 数值区间统一用 Low/High 和端点是否闭区间表示；字符串条件用 Op 解释 Str/Values。
// 指针字段在高频匹配路径上会带来额外访问，因此这里使用 Has* 布尔值。
type Clause struct {
	Attr string `json:"attr"`
	Op   Op     `json:"op"`

	// Numeric 区分同名 OpEqual 是数值等于还是字符串等于。
	Numeric bool `json:"numeric,omitempty"`

	// 字符串条件。
	Str    string   `json:"str,omitempty"`
	Values []string `json:"values,omitempty"`

	// 数值条件；Low/High 可能是 ±Inf。
	Low           float64 `json:"low,omitempty"`
	High          float64 `json:"high,omitempty"`
	HasLow        bool    `json:"has_low,omitempty"`
	HasHigh       bool    `json:"has_high,omitempty"`
	LowInclusive  bool    `json:"low_inclusive,omitempty"`
	HighInclusive bool    `json:"high_inclusive,omitempty"`
}

// Predicate 是若干 Clause 的合取。Subscription、合并后的 Filter 都实现这份结构。
type Predicate struct {
	Clauses []Clause `json:"clauses"`

	// MissingAsMatch=true 时，消息缺少条件提到的属性视为该条件通过；
	// false（默认）时视为整条不匹配。
	MissingAsMatch bool `json:"missing_as_match"`
}

// Subscription 是门店的一条原始订阅。
type Subscription struct {
	ID      string `json:"id"`
	StoreID string `json:"store_id"`
	Predicate
}

// Filter 是代理订阅表中的一条记录。它可能由若干原始订阅或下级上报合并而来。
type Filter struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id,omitempty"`

	Predicate

	// SourceIDs 指向下一级表中的直接来源记录，便于页面追踪合并血缘。
	SourceIDs []string `json:"source_ids,omitempty"`
	// LeafSubIDs 只在需要时填充；大规模运行中主要靠 SourceIDs 逐级追踪。
	LeafSubIDs []string `json:"leaf_sub_ids,omitempty"`

	MergedCount       int     `json:"merged_count"`
	BaseMatches       int     `json:"base_matches,omitempty"`
	FilterMatches     int     `json:"filter_matches,omitempty"`
	EstimatedOverhead float64 `json:"estimated_overhead,omitempty"`
	Label             string  `json:"label,omitempty"`
}

// Values 是一个属性的多值容器。数值和字符串属性分开存放，避免 interface{} 装箱。
type Values struct {
	Str []string  `json:"str,omitempty"`
	Num []float64 `json:"num,omitempty"`
}

// Message 是一条收当消息。
type Message struct {
	Seq      int               `json:"seq"`
	ID       string            `json:"id"`
	OriginID string            `json:"origin_id"`
	Attrs    map[string]Values `json:"attrs"`

	// Display 保存已经格式化好的核心属性，方便页面直接渲染。
	Display []AttrView `json:"display,omitempty"`
}

type AttrView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Value string `json:"value"`
}

func (m *Message) StringValues(attr string) ([]string, bool) {
	v, ok := m.Attrs[attr]
	if !ok || len(v.Str) == 0 {
		return nil, false
	}
	return v.Str, true
}

func (m *Message) NumericValues(attr string) ([]float64, bool) {
	v, ok := m.Attrs[attr]
	if !ok || len(v.Num) == 0 {
		return nil, false
	}
	return v.Num, true
}

// MatchClause 判断单个条件。属性多值时，任意一个值命中即可。
func MatchClause(c *Clause, m *Message) bool {
	if c.Numeric {
		vals, ok := m.NumericValues(c.Attr)
		if !ok {
			return false
		}
		for _, v := range vals {
			if matchNumericValue(c, v) {
				return true
			}
		}
		return false
	}

	vals, ok := m.StringValues(c.Attr)
	if !ok {
		// 调用方负责 missing policy；这里明确返回“属性缺失”由上层处理。
		return false
	}
	for _, v := range vals {
		if matchStringValue(c, v) {
			return true
		}
	}
	return false
}

// ClauseHasMissingAttr 区分“属性不存在”和“属性存在但值不满足”。
func ClauseAttrPresent(c *Clause, m *Message) bool {
	v, ok := m.Attrs[c.Attr]
	if !ok {
		return false
	}
	if c.Numeric {
		return len(v.Num) > 0
	}
	return len(v.Str) > 0
}

func matchStringValue(c *Clause, v string) bool {
	switch c.Op {
	case OpEqual:
		return v == c.Str
	case OpIn:
		for _, x := range c.Values {
			if v == x {
				return true
			}
		}
		return false
	case OpPrefix:
		return strings.HasPrefix(v, c.Str)
	case OpContains:
		return strings.Contains(v, c.Str)
	default:
		return false
	}
}

func matchNumericValue(c *Clause, v float64) bool {
	switch c.Op {
	case OpEqual:
		return c.HasLow && v == c.Low
	case OpGreater:
		return c.HasLow && v > c.Low
	case OpGTE:
		return c.HasLow && v >= c.Low
	case OpLess:
		return c.HasHigh && v < c.High
	case OpLTE:
		return c.HasHigh && v <= c.High
	case OpBetween:
		if c.HasLow {
			if c.LowInclusive {
				if v < c.Low {
					return false
				}
			} else if v <= c.Low {
				return false
			}
		}
		if c.HasHigh {
			if c.HighInclusive {
				if v > c.High {
					return false
				}
			} else if v >= c.High {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// Matches 是不依赖任何索引的直接判断实现，也是正确性核对的 oracle。
func (p Predicate) Matches(m *Message) bool {
	for i := range p.Clauses {
		c := &p.Clauses[i]
		v, ok := m.Attrs[c.Attr]
		present := ok
		if present {
			if c.Numeric {
				present = len(v.Num) > 0
			} else {
				present = len(v.Str) > 0
			}
		}
		if !present {
			if p.MissingAsMatch {
				continue
			}
			return false
		}
		if c.Numeric {
			hit := false
			for _, x := range v.Num {
				if matchNumericValue(c, x) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		} else {
			hit := false
			for _, x := range v.Str {
				if matchStringValue(c, x) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
	}
	return true
}

func (s Subscription) Matches(m *Message) bool { return s.Predicate.Matches(m) }

// Explain 返回条件的中文展示，供页面和命令行共用。
func (c Clause) Explain() string {
	switch c.Op {
	case OpEqual:
		return fmt.Sprintf("%s = %q", c.Attr, c.Str)
	case OpIn:
		return fmt.Sprintf("%s ∈ {%s}", c.Attr, strings.Join(c.Values, ","))
	case OpPrefix:
		return fmt.Sprintf("%s 前缀 %q", c.Attr, c.Str)
	case OpContains:
		return fmt.Sprintf("%s 包含 %q", c.Attr, c.Str)
	case OpGreater:
		return fmt.Sprintf("%s > %v", c.Attr, c.Low)
	case OpGTE:
		return fmt.Sprintf("%s ≥ %v", c.Attr, c.Low)
	case OpLess:
		return fmt.Sprintf("%s < %v", c.Attr, c.High)
	case OpLTE:
		return fmt.Sprintf("%s ≤ %v", c.Attr, c.High)
	case OpBetween:
		l := "("
		if c.LowInclusive {
			l = "["
		}
		r := ")"
		if c.HighInclusive {
			r = "]"
		}
		lo, hi := c.Low, c.High
		return fmt.Sprintf("%s ∈ %s%v, %v%s", c.Attr, l, lo, hi, r)
	default:
		return c.Attr + " ?"
	}
}
