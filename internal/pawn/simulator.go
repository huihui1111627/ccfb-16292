package pawn

import (
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type LatencyHistogram struct {
	BoundsNS []int64 `json:"bounds_ns"`
	Counts   []int64 `json:"counts"`
	Count    int64   `json:"count"`
	TotalNS  int64   `json:"total_ns"`
	MinNS    int64   `json:"min_ns"`
	MaxNS    int64   `json:"max_ns"`
	mu       sync.Mutex
}

// HistogramView 是可安全 JSON 编码的不可变快照，避免编码时与 Record 竞争。
type HistogramView struct {
	BoundsNS []int64 `json:"bounds_ns"`
	Counts   []int64 `json:"counts"`
	Count    int64   `json:"count"`
	TotalNS  int64   `json:"total_ns"`
	MinNS    int64   `json:"min_ns"`
	MaxNS    int64   `json:"max_ns"`
}

func (h *LatencyHistogram) View() HistogramView {
	h.mu.Lock()
	defer h.mu.Unlock()
	bounds := append([]int64(nil), h.BoundsNS...)
	counts := append([]int64(nil), h.Counts...)
	return HistogramView{
		BoundsNS: bounds, Counts: counts, Count: h.Count,
		TotalNS: h.TotalNS, MinNS: h.MinNS, MaxNS: h.MaxNS,
	}
}

func NewLatencyHistogram() *LatencyHistogram {
	bounds := []int64{
		500, 1_000, 2_500, 5_000, 10_000, 25_000, 50_000,
		100_000, 250_000, 500_000, 1_000_000, 5_000_000,
	}
	return &LatencyHistogram{BoundsNS: bounds, Counts: make([]int64, len(bounds)+1), MinNS: 1 << 62}
}

func (h *LatencyHistogram) Record(ns int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Count++
	h.TotalNS += ns
	if ns < h.MinNS {
		h.MinNS = ns
	}
	if ns > h.MaxNS {
		h.MaxNS = ns
	}
	i := sort.Search(len(h.BoundsNS), func(i int) bool { return ns <= h.BoundsNS[i] })
	h.Counts[i]++
}

func (h *LatencyHistogram) AvgNS() float64 {
	if h.Count == 0 {
		return 0
	}
	return float64(h.TotalNS) / float64(h.Count)
}

func (h *LatencyHistogram) Percentile(p float64) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Count == 0 {
		return 0
	}
	want := int64(float64(h.Count) * p)
	if want < 1 {
		want = 1
	}
	var acc int64
	for i, c := range h.Counts {
		acc += c
		if acc >= want {
			if i < len(h.BoundsNS) {
				return h.BoundsNS[i]
			}
			return h.MaxNS
		}
	}
	return h.MaxNS
}

type Benchmark struct {
	Subscriptions int     `json:"subscriptions"`
	Messages      int     `json:"messages"`
	TotalNS       int64   `json:"total_ns"`
	AvgMicros     float64 `json:"avg_micros"`
	P95Micros     float64 `json:"p95_micros"`
	MaxMicros     float64 `json:"max_micros"`
	Matched       int64   `json:"matched"`
}

type Simulation struct {
	Cfg     Config
	Gen     *Generator
	Planned []PlannedSub
	Changes map[int][]SubChange
	Net     *Network

	Masks   []uint64
	MatchNS []uint64

	Hist      *LatencyHistogram
	benchMu   sync.RWMutex
	Benchmark Benchmark

	progress atomic.Int64
	running  atomic.Bool
	done     atomic.Bool

	recentMu  sync.RWMutex
	traceRing []Trace
	tracePos  atomic.Uint64
}

func NewSimulation(cfg Config) (*Simulation, error) {
	gen := NewGenerator(cfg)
	planned := gen.PlannedSubs()
	changes := gen.Changes(planned)
	sample := gen.SampleMessages(cfg.SampleSize)
	net := NewNetwork(cfg, sample, 0.20)
	net.CacheSubBits(subscriptionSlice(planned))
	net.LoadInitial(planned)

	return &Simulation{
		Cfg:       cfg,
		Gen:       gen,
		Planned:   planned,
		Changes:   changes,
		Net:       net,
		Masks:     make([]uint64, cfg.MessageCount),
		MatchNS:   make([]uint64, cfg.MessageCount),
		Hist:      NewLatencyHistogram(),
		traceRing: make([]Trace, 160),
	}, nil
}

func subscriptionSlice(planned []PlannedSub) []Subscription {
	out := make([]Subscription, len(planned))
	for i := range planned {
		out[i] = planned[i].Sub
	}
	return out
}

func (s *Simulation) Progress() int64 { return s.progress.Load() }
func (s *Simulation) IsRunning() bool { return s.running.Load() }
func (s *Simulation) IsDone() bool    { return s.done.Load() }

func (s *Simulation) SetBenchmark(b Benchmark) {
	s.benchMu.Lock()
	s.Benchmark = b
	s.benchMu.Unlock()
}

func (s *Simulation) BenchmarkView() Benchmark {
	s.benchMu.RLock()
	defer s.benchMu.RUnlock()
	return s.Benchmark
}

func (s *Simulation) RecentTraces() []Trace {
	s.recentMu.RLock()
	out := make([]Trace, 0, len(s.traceRing))
	for _, tr := range s.traceRing {
		if tr.MessageID != "" {
			out = append(out, tr)
		}
	}
	s.recentMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// ReplayRandom 用新的时间种子生成一条消息并走当前表，供页面交互观察路径。
// 它不写入固定种子的 Masks，因此不影响正式核对结果。
func (s *Simulation) ReplayRandom() (uint64, Trace) {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	g := NewGenerator(s.Cfg)
	g.r = r
	msg := g.Message(int(r.Int63n(1_000_000)))
	mask, tr := s.Net.DispatchMessage(msg)
	s.addTrace(tr)
	return mask, tr
}

func (s *Simulation) addTrace(tr Trace) {
	pos := s.tracePos.Add(1) - 1
	s.recentMu.Lock()
	if len(s.traceRing) > 0 {
		s.traceRing[pos%uint64(len(s.traceRing))] = tr
	}
	s.recentMu.Unlock()
}

type proxyEvent struct {
	seq int
	run func(s *Simulation) string
}

func (s *Simulation) proxySchedule() []proxyEvent {
	n := s.Cfg.MessageCount
	at := func(f float64) int { return int(float64(n) * f) }
	events := []proxyEvent{
		{seq: at(0.12), run: func(s *Simulation) string {
			_ = s.Net.AddProxy("R9", "HQ")
			return "R9 join"
		}},
		{seq: at(0.25), run: func(s *Simulation) string {
			for _, id := range []string{"S01", "S02", "S03", "S04"} {
				if s.Net.HasNode(id) {
					_ = s.Net.MoveProxy(id, "R9")
				}
			}
			return "move S01-S04 to R9"
		}},
		{seq: at(0.40), run: func(s *Simulation) string {
			_ = s.Net.AddProxy("R10", "HQ")
			return "R10 join"
		}},
		{seq: at(0.70), run: func(s *Simulation) string {
			for _, id := range []string{"S05", "S06"} {
				if s.Net.HasNode(id) {
					_ = s.Net.MoveProxy(id, "R9")
				}
			}
			return "move S05-S06 to R9"
		}},
		{seq: at(0.83), run: func(s *Simulation) string {
			_ = s.Net.RemoveProxy("R10")
			return "R10 leave"
		}},
		{seq: at(0.94), run: func(s *Simulation) string {
			_ = s.Net.RemoveProxy("R9")
			return "R9 leave"
		}},
	}
	if s.Cfg.RegionCount >= 4 {
		events = append(events, proxyEvent{seq: at(0.55), run: func(s *Simulation) string {
			_ = s.Net.RemoveProxy("R4")
			return "R4 leave and reparent stores"
		}})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })
	return events
}

func (s *Simulation) Run() {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	defer s.running.Store(false)
	defer s.done.Store(true)

	schedule := s.proxySchedule()
	proxyAt := map[int][]proxyEvent{}
	boundarySet := map[int]struct{}{}
	for _, e := range schedule {
		proxyAt[e.seq] = append(proxyAt[e.seq], e)
		boundarySet[e.seq] = struct{}{}
	}
	for seq := range s.Changes {
		boundarySet[seq] = struct{}{}
	}
	boundaries := make([]int, 0, len(boundarySet))
	for seq := range boundarySet {
		if seq >= 0 && seq < s.Cfg.MessageCount {
			boundaries = append(boundaries, seq)
		}
	}
	sort.Ints(boundaries)

	pos := 0
	for _, seq := range boundaries {
		if seq > pos {
			s.dispatchRange(pos, seq)
		}
		if chs := s.Changes[seq]; len(chs) > 0 {
			s.Net.ApplyChanges(chs)
		}
		for _, e := range proxyAt[seq] {
			e.run(s)
		}
		pos = seq
	}
	if pos < s.Cfg.MessageCount {
		s.dispatchRange(pos, s.Cfg.MessageCount)
	}

	// 回放结束后做一次紧凑全量重算，让页面看到增删完成后的最终合并表。
	s.Net.Rebuild()
	s.SetBenchmark(s.BenchmarkGlobalMatcher(2000))
}

// dispatchRange 并发分发一个不含拓扑/订阅事件的连续消息区间。
// 固定种子的消息仍由主 goroutine 按 seq 顺序生成，匹配和路由则可跨核并行。
func (s *Simulation) dispatchRange(start, end int) {
	const chunk = 2048
	workers := runtime.GOMAXPROCS(0)
	if workers < 1 {
		workers = 1
	}

	for chunkStart := start; chunkStart < end; chunkStart += chunk {
		chunkEnd := chunkStart + chunk
		if chunkEnd > end {
			chunkEnd = end
		}
		msgs := make([]Message, chunkEnd-chunkStart)
		for i := range msgs {
			// Gen.Message 的 seq 参数主要用于 ID；随机流按调用顺序推进。
			msgs[i] = s.Gen.Message(chunkStart + i)
			msgs[i].Seq = chunkStart + i
		}

		var wg sync.WaitGroup
		jobs := make(chan int)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					msg := &msgs[i]
					seq := chunkStart + i
					mask, tr := s.Net.DispatchMessage(*msg)
					s.Masks[seq] = mask
					s.MatchNS[seq] = uint64(tr.MatchNS)
					s.Hist.Record(tr.MatchNS)
					s.addTrace(tr)
					// 并发完成顺序不同于 seq，因此用完成计数而不是 Store(seq+1)。
					s.progress.Add(1)
				}
			}()
		}
		for i := range msgs {
			jobs <- i
		}
		close(jobs)
		wg.Wait()

		if s.Cfg.ProgressEvery > 0 && chunkEnd%s.Cfg.ProgressEvery < chunk {
			fmt.Printf("[sim] %d/%d messages\n", chunkEnd, s.Cfg.MessageCount)
		}
	}
}

// BenchmarkGlobalMatcher 用全部几十万条原始订阅构建一个中央索引，单独给出匹配耗时，
// 用来证明候选索引而不是全表扫描的性能。实际分发使用各级代理合并后的表。
func (s *Simulation) BenchmarkGlobalMatcher(messageN int) Benchmark {
	preds := make([]Predicate, len(s.Planned))
	for i := range s.Planned {
		preds[i] = s.Planned[i].Sub.Predicate
	}
	m := NewMatcher(preds)

	msgs := make([]Message, messageN)
	br := rand.New(rand.NewSource(s.Cfg.Seed + 424242))
	old := s.Gen.r
	s.Gen.r = br
	for i := 0; i < messageN; i++ {
		msgs[i] = s.Gen.Message(30_000_000 + i)
	}
	s.Gen.r = old

	durations := make([]int64, 0, messageN)
	var matched int64
	startAll := time.Now()
	for i := range msgs {
		st := time.Now()
		hs := m.Match(&msgs[i])
		durations = append(durations, time.Since(st).Nanoseconds())
		matched += int64(len(hs))
	}
	total := time.Since(startAll).Nanoseconds()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[int(float64(len(durations)-1)*0.95)]
	max := durations[len(durations)-1]
	return Benchmark{
		Subscriptions: len(s.Planned),
		Messages:      messageN,
		TotalNS:       total,
		AvgMicros:     float64(total) / float64(messageN) / 1000.0,
		P95Micros:     float64(p95) / 1000.0,
		MaxMicros:     float64(max) / 1000.0,
		Matched:       matched,
	}
}
