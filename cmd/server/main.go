package main

import (
	"log"
	"os"

	"evacsvc/internal/core"
	"evacsvc/internal/server"
)

func main() {
	dbPath := os.Getenv("EVAC_DB")
	if dbPath == "" {
		dbPath = "evac.db"
	}
	addr := os.Getenv("EVAC_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	db, err := core.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	if err := core.Seed(db); err != nil {
		log.Fatalf("注入演示数据失败: %v", err)
	}

	app := server.New(core.NewService(db, nil))
	log.Printf("勤务区撤离放行服务启动，监听 %s，数据库 %s", addr, dbPath)
	log.Printf("演示账号: gc-01(地面保障指挥) lc-01(发射指挥) lead-fuel/lead-fire/lead-measure(班组长) ctr-alpha/ctr-beta(承包单位) gate-sys(门禁)")
	if err := app.Listen(addr); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
