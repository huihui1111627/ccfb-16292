package pawn

import (
	"math"
	"testing"
)

func TestBoundarySemantics(t *testing.T) {
	cases := []struct {
		name string
		c    Clause
		want bool
	}{
		{"open-low-excludes-boundary", Clause{Attr: "w", Numeric: true, Op: OpBetween, Low: 20, HasLow: true, LowInclusive: false, High: 100, HasHigh: true, HighInclusive: true}, false},
		{"closed-low-includes-boundary", Clause{Attr: "w", Numeric: true, Op: OpBetween, Low: 20, HasLow: true, LowInclusive: true, High: 100, HasHigh: true, HighInclusive: false}, true},
		{"open-high-excludes-boundary", Clause{Attr: "w", Numeric: true, Op: OpBetween, Low: 1, HasLow: true, LowInclusive: true, High: 20, HasHigh: true, HighInclusive: false}, false},
		{"closed-high-includes-boundary", Clause{Attr: "w", Numeric: true, Op: OpLTE, High: 20, HasHigh: true, HighInclusive: true}, true},
		{"gt20-excludes-20", Clause{Attr: "w", Numeric: true, Op: OpGreater, Low: 20, HasLow: true}, false},
		{"gte20-includes-20", Clause{Attr: "w", Numeric: true, Op: OpGTE, Low: 20, HasLow: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchNumericValue(&tc.c, 20)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMultiValueAndMissingPolicy(t *testing.T) {
	msg := &Message{Attrs: map[string]Values{
		AttrCategory: {Str: []string{"金饰", "古董"}},
	}}
	catGold := Clause{Attr: AttrCategory, Op: OpEqual, Str: "古董"}
	if !MatchClause(&catGold, msg) {
		t.Fatal("multi-value category should hit one value")
	}

	withWeight := Predicate{Clauses: []Clause{
		{Attr: AttrCategory, Op: OpEqual, Str: "金饰"},
		{Attr: AttrGoldWeight, Numeric: true, Op: OpGreater, Low: 20, HasLow: true},
	}}
	if withWeight.Matches(msg) {
		t.Fatal("missing numeric attr should mismatch by default")
	}
	withWeight.MissingAsMatch = true
	if !withWeight.Matches(msg) {
		t.Fatal("missing attr should be treated as match when configured")
	}
}

func TestMatcherMatchesDirectOracle(t *testing.T) {
	cfg := SmallConfig()
	cfg.InitialPerStore = 80
	cfg.ExtraSubs = 120
	cfg.MessageCount = 2000
	g := NewGenerator(cfg)
	planned := g.PlannedSubs()
	preds := make([]Predicate, len(planned))
	for i := range planned {
		preds[i] = planned[i].Sub.Predicate
	}
	m := NewMatcher(preds)

	for seq := 0; seq < 1000; seq++ {
		msg := g.Message(seq)
		gotHandles := m.Match(&msg)
		got := map[int32]bool{}
		for _, h := range gotHandles {
			got[h] = true
		}
		for i := range preds {
			want := preds[i].Matches(&msg)
			if want != got[int32(i)] {
				t.Fatalf("seq=%d sub=%d id=%s matcher=%v oracle=%v", seq, i, planned[i].Sub.ID, got[int32(i)], want)
			}
		}
	}
}

func TestMergeFiltersCoverInputs(t *testing.T) {
	cfg := SmallConfig()
	cfg.InitialPerStore = 120
	cfg.ExtraSubs = 0
	g := NewGenerator(cfg)
	planned := g.PlannedSubs()
	sample := g.SampleMessages(64)
	inputs := make([]Filter, len(planned))
	byID := map[string]Subscription{}
	for i, p := range planned {
		inputs[i] = subToFilter(p.Sub)
		byID[p.Sub.ID] = p.Sub
	}
	merged := MergeFilters(inputs, MergeOptions{Tolerance: 0.5, Sample: sample, IDPrefix: "TEST"})
	if len(merged) >= len(inputs) {
		t.Fatalf("expected merge reduction: %d -> %d", len(inputs), len(merged))
	}

	idToOutput := map[string]Filter{}
	for _, f := range merged {
		ids := sourceIDsOrSelf(f)
		for _, id := range ids {
			idToOutput[id] = f
		}
	}

	checkGen := NewGenerator(cfg)
	_ = checkGen.PlannedSubs()
	for seq := 0; seq < 500; seq++ {
		msg := checkGen.Message(seq)
		for _, p := range planned {
			if !p.Sub.Matches(&msg) {
				continue
			}
			out, ok := idToOutput[p.Sub.ID]
			if !ok {
				t.Fatalf("sub %s missing merged output", p.Sub.ID)
			}
			if !out.Matches(&msg) {
				t.Fatalf("merged filter %s does not cover sub %s at seq %d\nsub=%#v\nout=%#v\nmsg=%#v", out.ID, p.Sub.ID, seq, p.Sub.Predicate, out.Predicate, msg.Attrs)
			}
		}
	}
}

func TestEndToEndDirectExactSmall(t *testing.T) {
	cfg := SmallConfig()
	cfg.MessageCount = 4000
	sim, err := NewSimulation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sim.Run()
	rep := VerifyDirect(sim, cfg.MessageCount)
	if !rep.OK {
		for i, v := range rep.Violations {
			if i >= 10 {
				break
			}
			t.Errorf("violation %+v", v)
		}
		t.Fatalf("direct verification failed: %d violations", len(rep.Violations))
	}
	t.Logf("verified %d messages x %d stores, %d active evaluations in %dms",
		rep.MessagesChecked, rep.StoresChecked, rep.ActivePairsEvaluated, rep.ElapsedMS)
}

func TestEmptyStringGeneralizationAnchor(t *testing.T) {
	p := Predicate{Clauses: []Clause{
		{Attr: AttrBrand, Op: OpPrefix, Str: ""},
		{Attr: AttrCategory, Op: OpEqual, Str: "名表"},
	}}
	m := NewMatcher([]Predicate{p})
	withBrand := &Message{Attrs: map[string]Values{
		AttrBrand:    {Str: []string{"Rolex"}},
		AttrCategory: {Str: []string{"名表"}},
	}}
	if hs := m.Match(withBrand); len(hs) != 1 {
		t.Fatalf("empty prefix should index as wildcard, got %d", len(hs))
	}
	missingBrand := &Message{Attrs: map[string]Values{
		AttrCategory: {Str: []string{"名表"}},
	}}
	if hs := m.Match(missingBrand); len(hs) != 0 {
		t.Fatalf("missing brand should mismatch when MissingAsMatch=false")
	}
	p.MissingAsMatch = true
	m = NewMatcher([]Predicate{p})
	if hs := m.Match(missingBrand); len(hs) != 1 {
		t.Fatalf("missing brand should pass under missing-as-match")
	}
}

func TestMatcherTautologyAndTypedMissing(t *testing.T) {
	emptyPred := Predicate{}
	m := NewMatcher([]Predicate{emptyPred})
	msg := &Message{Attrs: map[string]Values{}}
	if hs := m.Match(msg); len(hs) != 1 || hs[0] != 0 {
		t.Fatalf("empty predicate must match everything, handles=%v", hs)
	}

	numMissing := Predicate{MissingAsMatch: true, Clauses: []Clause{{
		Attr: "x", Numeric: true, Op: OpGTE, Low: 1, HasLow: true,
	}}}
	m = NewMatcher([]Predicate{numMissing})
	wrongType := &Message{Attrs: map[string]Values{"x": {Str: []string{"z"}}}}
	if !numMissing.Matches(wrongType) {
		t.Fatal("oracle should treat numeric values as type-missing under missing policy")
	}
	if hs := m.Match(wrongType); len(hs) != 1 {
		t.Fatalf("typed missing numeric anchor omitted, handles=%v", hs)
	}

	strMissing := Predicate{MissingAsMatch: true, Clauses: []Clause{{Attr: "x", Op: OpEqual, Str: "a"}}}
	numOnly := &Message{Attrs: map[string]Values{"x": {Num: []float64{1}}}}
	m = NewMatcher([]Predicate{
		strMissing,
		{Clauses: []Clause{{Attr: "x", Numeric: true, Op: OpGTE, Low: 0, HasLow: true}}},
	})
	hs := m.Match(numOnly)
	got := map[int32]bool{}
	for _, h := range hs {
		got[h] = true
	}
	if !got[0] || !got[1] {
		t.Fatalf("string missing-policy anchor and numeric predicate should both match, handles=%v", hs)
	}
}

func TestNestedReparentAndCycleRejected(t *testing.T) {
	cfg := SmallConfig()
	cfg.MessageCount = 100
	sim, err := NewSimulation(cfg)
	if err != nil {
		t.Fatal(err)
	}
	net := sim.Net
	alwaysMatch := Subscription{ID: "X-ALWAYS", StoreID: "S01", Predicate: Predicate{}}
	net.ApplyChanges([]SubChange{{Add: true, Sub: alwaysMatch}})

	if err := net.AddProxy("N1", "R1"); err != nil {
		t.Fatal(err)
	}
	if err := net.AddProxy("N2", "N1"); err != nil {
		t.Fatal(err)
	}
	if err := net.MoveProxy("S01", "N2"); err != nil {
		t.Fatal(err)
	}
	msg := Message{Seq: 1, ID: "M1", OriginID: "S02", Attrs: map[string]Values{}}
	mask, _ := net.DispatchMessage(msg)
	if mask&1 == 0 {
		t.Fatal("nested reparent left stale ancestor table and dropped S01")
	}
	if err := net.MoveProxy("HQ", "R1"); err == nil {
		t.Fatal("moving root should be rejected")
	}
	if err := net.MoveProxy("R1", "N1"); err == nil {
		t.Fatal("moving proxy below its own descendant should be rejected")
	}
}

func TestIntervalTreeBasic(t *testing.T) {
	tr := newIntervalTree()
	tr.Insert(1, interval{low: 10, high: 20, lowInc: false, highInc: false})
	tr.Insert(2, interval{low: 20, high: 30, lowInc: true, highInc: true})
	tr.Insert(3, interval{low: 0, high: math.Inf(1), lowInc: true, highInc: true})
	got := map[int32]bool{}
	tr.Query(20, func(id int32) { got[id] = true })
	if got[1] {
		t.Fatal("open high interval should not contain boundary")
	}
	if !got[2] || !got[3] {
		t.Fatalf("expected 2 and 3, got %v", got)
	}
}
