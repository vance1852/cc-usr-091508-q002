// 勤务区撤离放行服务入口。
package main

import (
	"flag"
	"log"
	"time"

	"evac/internal/api"
	"evac/internal/domain"
	"evac/internal/service"
	"evac/internal/store"
)

func main() {
	var (
		dbPath = flag.String("db", "evac.db", "SQLite 数据库文件路径")
		addr   = flag.String("addr", ":8080", "监听地址")
		seed   = flag.Bool("seed", false, "写入演示数据（危险区、班组、名册、车辆）")
	)
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	svc := service.New(st)
	if *seed {
		if err := seedData(svc); err != nil {
			log.Fatalf("写入演示数据失败: %v", err)
		}
		log.Println("演示数据已写入")
	}

	app := api.NewApp(svc)
	log.Printf("勤务区撤离放行服务监听 %s", *addr)
	log.Fatal(app.Listen(*addr))
}

// seedData 写入一套转运就位后的典型现场数据：
// 加注区 Z-FUEL 与发射工位区 Z-PAD（依赖加注区），加注/消防/测量三个班组，
// 含跨午夜班次人员与活动门架。
func seedData(svc *service.Service) error {
	if _, err := svc.CreateZone("Z-FUEL", "加注作业区"); err != nil {
		return err
	}
	if _, err := svc.CreateZone("Z-PAD", "发射工位区"); err != nil {
		return err
	}
	if _, err := svc.PublishZoneVersion("Z-FUEL", "半径300m", false); err != nil {
		return err
	}
	if _, err := svc.PublishZoneVersion("Z-PAD", "半径500m", false); err != nil {
		return err
	}
	if err := svc.AddDependency("Z-PAD", "Z-FUEL"); err != nil {
		return err
	}
	for _, u := range [][2]string{
		{"U-FUEL", "加注承包一队"}, {"U-FIRE", "消防保障分队"}, {"U-SURV", "测量技术室"},
	} {
		if err := svc.RegisterUnit(u[0], u[1]); err != nil {
			return err
		}
	}
	for _, c := range []domain.Crew{
		{ID: "C-FUEL-1", Name: "加注一班", UnitID: "U-FUEL", ZoneID: "Z-FUEL", LeadUserID: "lead-fuel-1"},
		{ID: "C-FIRE-1", Name: "消防一班", UnitID: "U-FIRE", ZoneID: "Z-FUEL", LeadUserID: "lead-fire-1"},
		{ID: "C-SURV-1", Name: "测量一班", UnitID: "U-SURV", ZoneID: "Z-PAD", LeadUserID: "lead-surv-1"},
	} {
		if err := svc.RegisterCrew(c); err != nil {
			return err
		}
	}
	for _, p := range []domain.Person{
		{ID: "P-001", Name: "张加注", Post: "加注操作手", UnitID: "U-FUEL", CrewID: "C-FUEL-1", ZoneID: "Z-FUEL", ShiftStart: "08:00", ShiftEnd: "20:00"},
		{ID: "P-002", Name: "李夜班", Post: "加注巡检", UnitID: "U-FUEL", CrewID: "C-FUEL-1", ZoneID: "Z-FUEL", ShiftStart: "22:00", ShiftEnd: "06:00"},
		{ID: "P-003", Name: "王消防", Post: "消防值守", UnitID: "U-FIRE", CrewID: "C-FIRE-1", ZoneID: "Z-FUEL", ShiftStart: "00:00", ShiftEnd: "00:00"},
		{ID: "P-004", Name: "赵测量", Post: "瞄准测量", UnitID: "U-SURV", CrewID: "C-SURV-1", ZoneID: "Z-PAD", ShiftStart: "08:00", ShiftEnd: "20:00"},
	} {
		if err := svc.RegisterPerson(p); err != nil {
			return err
		}
	}
	for _, a := range []domain.Asset{
		{ID: "V-101", Label: "推进剂槽车 101", Kind: "vehicle", UnitID: "U-FUEL", ZoneID: "Z-FUEL"},
		{ID: "G-201", Label: "活动门架 201", Kind: "gantry", UnitID: "U-SURV", ZoneID: "Z-PAD"},
	} {
		if err := svc.RegisterAsset(a); err != nil {
			return err
		}
	}
	// 演示一笔离场回执
	_, _, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "SCAN-DEMO-1", SubjectKind: "person", SubjectID: "P-001",
		Gate: "GATE-3", Direction: domain.DirExit, ScannedAt: time.Now().UTC().Add(-30 * time.Minute),
	})
	return err
}
