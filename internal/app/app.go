package app

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"pawnrouter/internal/pawn"
	"pawnrouter/internal/web"
)

// Main 是 server 二进制和仓库根目录 `go run .` 共用的入口。
func Main() {
	addrDefault := ":8088"
	if p := os.Getenv("PORT"); p != "" {
		addrDefault = ":" + p
	} else if a := os.Getenv("ADDR"); a != "" {
		addrDefault = a
	}

	addr := flag.String("addr", addrDefault, "HTTP 监听地址")
	scale := flag.String("scale", "full", "full=240k订阅/240万消息，small=快速冒烟")
	msgs := flag.Int("messages", 0, "覆盖消息数（0 表示使用规模默认值）")
	stores := flag.Int("stores", 0, "覆盖门店数")
	port := flag.Int("port", 0, "覆盖端口的便捷参数")
	flag.Parse()

	cfg := pawn.DefaultConfig()
	if *scale == "small" || os.Getenv("PAWN_SCALE") == "small" {
		cfg = pawn.SmallConfig()
	}
	if *msgs > 0 {
		cfg.MessageCount = *msgs
	}
	if *stores > 0 {
		if *stores > 64 {
			log.Printf("门店位掩码演示最多支持 64 家，-stores=%d 已截断为 64", *stores)
			*stores = 64
		}
		cfg.StoreCount = *stores
		cfg.InitialPerStore = 200
		cfg.ExtraSubs = 300
		cfg.DeleteCount = 100
	}
	if *port > 0 {
		*addr = fmt.Sprintf(":%d", *port)
	}

	server := web.NewServer()
	go func() {
		log.Printf("正在生成固定种子数据并构建订阅索引：seed=%d stores=%d initial/s=%d messages=%d",
			cfg.Seed, cfg.StoreCount, cfg.InitialPerStore, cfg.MessageCount)
		t0 := time.Now()
		sim, err := pawn.NewSimulation(cfg)
		if err != nil {
			log.Fatalf("初始化仿真失败: %v", err)
		}
		log.Printf("订阅表和合并表构建完成，用时 %s；开始回放消息", time.Since(t0))
		server.SetSimulation(sim)
		sim.Run()
		log.Printf("消息回放完成：%d 条；可在页面触发抽样直判核对，或运行 cmd/verify 做全量核对", cfg.MessageCount)
	}()

	listenURL := *addr
	if len(listenURL) > 0 && listenURL[0] == ':' {
		listenURL = "http://localhost" + listenURL
	} else {
		listenURL = "http://" + listenURL
	}
	log.Printf("收当订阅分发中间件页面: %s", listenURL)
	if err := http.ListenAndServe(*addr, server.Handler()); err != nil {
		log.Fatal(err)
	}
}
