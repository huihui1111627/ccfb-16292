package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"text/tabwriter"
	"time"

	"pawnrouter/internal/pawn"
)

func main() {
	scale := flag.String("scale", "full", "full 或 small")
	messages := flag.Int("messages", 0, "覆盖消息数")
	jsonOut := flag.Bool("json", false, "输出 JSON 快照")
	flag.Parse()

	cfg := pawn.DefaultConfig()
	if *scale == "small" {
		cfg = pawn.SmallConfig()
	}
	if *messages > 0 {
		cfg.MessageCount = *messages
	}

	t0 := time.Now()
	sim, err := pawn.NewSimulation(cfg)
	if err != nil {
		log.Fatal(err)
	}
	sim.Run()
	snap := sim.Net.Snapshot(0)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"elapsed_ms": time.Since(t0).Milliseconds(),
			"config":     cfg,
			"benchmark":  sim.Benchmark,
			"network":    snap,
		})
		return
	}

	fmt.Printf("规模: subs=%d messages=%d stores=%d nodes=%d links=%d elapsed=%s\n\n",
		cfg.InitialPerStore*cfg.StoreCount+cfg.ExtraSubs, cfg.MessageCount, cfg.StoreCount,
		len(snap.Nodes), len(snap.Links), time.Since(t0).Round(time.Millisecond))

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tLEVEL\tRAW\tRECEIVED\tREPORTED\tAVG_MATCH_US")
	for _, n := range snap.Nodes {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%.2f\n", n.ID, n.Level, n.RawSubCount, n.ReceivedCount, n.ReportedCount, n.Stats.AvgMatchMicros)
	}
	w.Flush()

	fmt.Println()
	w = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "LINK\tUP\tDOWN\tNECESSARY\tEXTRA\tOVERHEAD")
	var up, down, necessary, extra int64
	allLinks := append(append([]pawn.LinkView{}, snap.Links...), snap.RetiredLinks...)
	for _, l := range allLinks {
		up += l.Up
		down += l.Down
		necessary += l.Necessary
		extra += l.Extra
		fmt.Fprintf(w, "%s->%s%s\t%d\t%d\t%d\t%d\t%.1f%%\n", l.ParentID, l.ChildID, retiredMark(l.Retired), l.Up, l.Down, l.Necessary, l.Extra, 100*l.OverheadRatio)
	}
	w.Flush()
	fmt.Printf("\nTOTAL links: up=%d down=%d necessary=%d extra=%d overhead=%.2f%%\n",
		up, down, necessary, extra, 100*safeRatio(extra, necessary))
	fmt.Printf("BENCHMARK full-index: subs=%d avg=%.3fus p95=%.3fus max=%.3fus matched=%d\n",
		sim.Benchmark.Subscriptions, sim.Benchmark.AvgMicros, sim.Benchmark.P95Micros,
		sim.Benchmark.MaxMicros, sim.Benchmark.Matched)
}

func safeRatio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func retiredMark(retired bool) string {
	if retired {
		return " (已结束)"
	}
	return ""
}
