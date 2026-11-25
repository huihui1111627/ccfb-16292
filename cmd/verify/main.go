package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"pawnrouter/internal/pawn"
)

func main() {
	scale := flag.String("scale", "full", "full=240k订阅/240万消息，small=快速冒烟")
	messages := flag.Int("messages", 0, "核对消息数；0 表示全量")
	runSimMessages := flag.Int("sim-messages", 0, "覆盖仿真消息数（调试用）")
	jsonOut := flag.Bool("json", false, "只输出 JSON 报告")
	flag.Parse()

	cfg := pawn.DefaultConfig()
	if *scale == "small" {
		cfg = pawn.SmallConfig()
	}
	if *runSimMessages > 0 {
		cfg.MessageCount = *runSimMessages
	}

	log.Printf("生成固定种子数据并运行仿真: subs≈%d messages=%d",
		cfg.InitialPerStore*cfg.StoreCount+cfg.ExtraSubs, cfg.MessageCount)
	t0 := time.Now()
	sim, err := pawn.NewSimulation(cfg)
	if err != nil {
		log.Fatal(err)
	}
	sim.Run()
	log.Printf("仿真完成，用时 %s；开始逐条直判核对", time.Since(t0))

	limit := *messages
	if limit == 0 && *scale == "small" {
		limit = cfg.MessageCount
	}
	rep := pawn.VerifyDirect(sim, limit)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if *jsonOut {
		_ = enc.Encode(rep)
	} else {
		fmt.Printf("\n核对结果: OK=%v\n", rep.OK)
		fmt.Printf("消息数=%d 门店数=%d 活跃条件判断次数=%d 用时=%dms\n",
			rep.MessagesChecked, rep.StoresChecked, rep.ActivePairsEvaluated, rep.ElapsedMS)
		if sim.Benchmark.Subscriptions > 0 {
			fmt.Printf("全量索引匹配基准: %d 订阅, avg=%.3fµs/msg, p95=%.3fµs, max=%.3fµs\n",
				sim.Benchmark.Subscriptions, sim.Benchmark.AvgMicros, sim.Benchmark.P95Micros, sim.Benchmark.MaxMicros)
		}
		if !rep.OK {
			for i, v := range rep.Violations {
				if i >= 20 {
					break
				}
				fmt.Printf("违规: %+v\n", v)
			}
			os.Exit(1)
		}
	}
	if !rep.OK {
		os.Exit(1)
	}
}
