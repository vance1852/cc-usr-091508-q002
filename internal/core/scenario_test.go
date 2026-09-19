package core_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"evacsvc/internal/core"
)

// t0 场景基准时间：2026-09-18 23:00 UTC，夜班（22:00–次日 06:00）进行中。
var t0 = time.Date(2026, 9, 18, 23, 0, 0, 0, time.UTC).Unix()

var receiptSeq atomic.Int64

func nextReceipt() string { return fmt.Sprintf("RCPT-%d", receiptSeq.Add(1)) }

type fixture struct {
	t    *testing.T
	db   *sql.DB
	svc  *core.Service
	path string
	now  atomic.Int64
}

func newFixtureAt(t *testing.T, path string) *fixture {
	t.Helper()
	db, err := core.Open(path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	f := &fixture{t: t, db: db, path: path}
	f.now.Store(t0)
	f.svc = core.NewService(db, func() time.Time { return time.Unix(f.now.Load(), 0) })
	return f
}

func newFixture(t *testing.T) *fixture {
	return newFixtureAt(t, filepath.Join(t.TempDir(), "test.db"))
}

func (f *fixture) close() { f.db.Close() }

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

// setupStandard 标准场景：
//   - 单位 u1/u2，班组 c1(u1)/c2(u2)
//   - 区域 Z1、Z2，Z2 依赖 Z1
//   - Z1 派遣：P1(c1)、V1(c1)；Z2 派遣：P2(c2)
//   - 跨午夜夜班 shift-night：2026-09-18 22:00 → 2026-09-19 06:00
func (f *fixture) setupStandard() {
	f.t.Helper()
	f.exec(`INSERT INTO units (id, name) VALUES ('u1','单位一'), ('u2','单位二')`)
	f.exec(`INSERT INTO crews (id, name, unit_id, specialty) VALUES
		('c1','班组一','u1','fueling'), ('c2','班组二','u2','measurement')`)
	f.exec(`INSERT INTO actors (id, name, role, unit_id, crew_id) VALUES
		('gc','地面保障指挥','ground_commander','',''),
		('lc','发射指挥','launch_commander','',''),
		('lead1','班组长一','crew_lead','u1','c1'),
		('lead2','班组长二','crew_lead','u2','c2'),
		('ctr1','单位一调度','contractor','u1',''),
		('gate','门禁系统','gate_operator','','')`)
	f.exec(`INSERT INTO zones (id, name) VALUES ('Z1','区域一'), ('Z2','区域二')`)
	f.exec(`INSERT INTO zone_deps (zone_id, depends_on) VALUES ('Z2','Z1')`)
	f.exec(`INSERT INTO zone_versions (zone_id, version, boundary, expanded, created_at) VALUES
		('Z1',1,'Z1 初始边界',0,?), ('Z2',1,'Z2 初始边界',0,?)`, t0-7200, t0-7200)
	f.exec(`INSERT INTO personnel (id, name, unit_id, crew_id, post) VALUES
		('P1','张三','u1','c1','操作手'), ('P2','李四','u2','c2','测量员')`)
	f.exec(`INSERT INTO vehicles (id, plate, kind, unit_id, crew_id) VALUES
		('V1','京A0001','加注车','u1','c1')`)
	f.exec(`INSERT INTO shifts (id, name, starts_at, ends_at) VALUES
		('shift-night','夜班',?,?)`,
		time.Date(2026, 9, 18, 22, 0, 0, 0, time.UTC).Unix(),
		time.Date(2026, 9, 19, 6, 0, 0, 0, time.UTC).Unix())
	f.exec(`INSERT INTO assignments (zone_id, object_type, object_id, crew_id, unit_id, shift_id) VALUES
		('Z1','person','P1','c1','u1','shift-night'),
		('Z1','vehicle','V1','c1','u1','shift-night'),
		('Z2','person','P2','c2','u2','shift-night')`)
	f.exec(`INSERT INTO milestones (id, name, seq) VALUES ('M1','转运就位',1)`)
}

func (f *fixture) actor(id string) core.Actor {
	f.t.Helper()
	a, err := f.svc.GetActor(id)
	if err != nil {
		f.t.Fatalf("actor %s: %v", id, err)
	}
	return a
}

func (f *fixture) gate(receipt, zone string, ot core.ObjectType, obj string, dir core.Direction, at int64) core.GateEvent {
	f.t.Helper()
	ev, err := f.svc.RecordGateEvent(core.GateEvent{
		ReceiptNo: receipt, GateID: "GATE-1", ZoneID: zone,
		ObjectType: ot, ObjectID: obj, Direction: dir, OccurredAt: at,
	})
	if err != nil {
		f.t.Fatalf("门禁回执 %s: %v", receipt, err)
	}
	return ev
}

// evacuate 为区域内全部派遣对象补齐 进+出 回执，使区域达到可确认状态。
func (f *fixture) evacuate(zone string) {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT object_type, object_id FROM assignments WHERE zone_id = ? AND active = 1`, zone)
	if err != nil {
		f.t.Fatalf("查询派遣: %v", err)
	}
	type obj struct{ t, id string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.t, &o.id); err != nil {
			f.t.Fatalf("扫描派遣: %v", err)
		}
		objs = append(objs, o)
	}
	rows.Close()
	for _, o := range objs {
		f.gate(nextReceipt(), zone, core.ObjectType(o.t), o.id, core.DirIn, t0-3600)
		f.gate(nextReceipt(), zone, core.ObjectType(o.t), o.id, core.DirOut, t0-600)
	}
}

func statusOf(err error) int {
	var ae *core.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func mustConflict(t *testing.T, err error, what string) {
	t.Helper()
	if statusOf(err) != 409 {
		t.Fatalf("%s: 期望 409，实际 err=%v", what, err)
	}
}

func freezeReasons(freezes []core.Freeze) map[string]int {
	out := map[string]int{}
	for _, fr := range freezes {
		out[fr.Reason]++
	}
	return out
}

// ---------------------------------------------------------------------------
// 场景一：重复扫码
// ---------------------------------------------------------------------------

func TestDuplicateScanIdempotent(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	first := f.gate("RCPT-DUP-1", "Z1", core.ObjectPerson, "P1", core.DirIn, t0-1000)
	if first.Duplicate {
		t.Fatal("首次录入不应标记为重复")
	}
	// 闸机重传同一回执号
	second := f.gate("RCPT-DUP-1", "Z1", core.ObjectPerson, "P1", core.DirIn, t0-1000)
	if !second.Duplicate {
		t.Fatal("重复扫码应标记 duplicate")
	}
	if second.ID != first.ID {
		t.Fatalf("重复扫码应返回同一记录: %d != %d", second.ID, first.ID)
	}
	var n, net int
	if err := f.db.QueryRow(`SELECT COUNT(*), SUM(CASE direction WHEN 'in' THEN 1 ELSE -1 END)
		FROM gate_events WHERE object_id = 'P1'`).Scan(&n, &net); err != nil {
		t.Fatal(err)
	}
	if n != 1 || net != 1 {
		t.Fatalf("重复扫码不得重复计数: events=%d net=%d", n, net)
	}
}

func TestConcurrentDuplicateScans(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	const goroutines = 32
	var wg sync.WaitGroup
	dups := atomic.Int64{}
	errs := atomic.Int64{}
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := f.svc.RecordGateEvent(core.GateEvent{
				ReceiptNo: "RCPT-STORM-1", GateID: "GATE-1", ZoneID: "Z1",
				ObjectType: core.ObjectVehicle, ObjectID: "V1",
				Direction: core.DirIn, OccurredAt: t0 - 900,
			})
			if err != nil {
				errs.Add(1)
				return
			}
			if ev.Duplicate {
				dups.Add(1)
			}
		}()
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("并发重复扫码不应报错: %d 个错误", errs.Load())
	}
	if dups.Load() != goroutines-1 {
		t.Fatalf("应有且仅有 1 条生效、%d 条判重，实际判重 %d", goroutines-1, dups.Load())
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM gate_events WHERE receipt_no = 'RCPT-STORM-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("并发重复扫码后应只有 1 条记录，实际 %d", n)
	}
}

// ---------------------------------------------------------------------------
// 场景二：并发确认
// ---------------------------------------------------------------------------

func TestConcurrentCrewConfirmations(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()
	// Z1 增加班组二的对象，使两个班组都需确认 Z1。
	f.exec(`INSERT INTO assignments (zone_id, object_type, object_id, crew_id, unit_id, shift_id)
		VALUES ('Z1','person','P2','c2','u2','shift-night')`)
	f.evacuate("Z1")

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	// 两个班组负责人并发确认，且各自重复提交（网络重试）。
	for i := 0; i < 4; i++ {
		for _, lead := range []struct{ actor, crew string }{{"lead1", "c1"}, {"lead2", "c2"}} {
			wg.Add(1)
			go func(actorID, crewID string) {
				defer wg.Done()
				a, err := f.svc.GetActor(actorID)
				if err != nil {
					errs <- err
					return
				}
				_, err = f.svc.ConfirmCrew(a, "Z1", crewID, "本组已撤离")
				errs <- err
			}(lead.actor, lead.crew)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发确认不应失败: %v", err)
		}
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM crew_confirmations WHERE zone_id = 'Z1' AND zone_version = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("每班组应只有一条确认，实际 %d", n)
	}
	// 两个班组均已确认后，地面保障指挥可核对。
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); err != nil {
		t.Fatalf("全部确认后应可核对: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 场景三：跨午夜班次
// ---------------------------------------------------------------------------

func TestCrossMidnightShift(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	day1 := time.Date(2026, 9, 18, 23, 59, 0, 0, time.UTC).Unix()
	day2 := time.Date(2026, 9, 19, 0, 30, 0, 0, time.UTC).Unix()

	// 跨午夜：23:59 进入，次日 00:30 离开，都应归属夜班。
	f.gate("RCPT-NIGHT-IN", "Z1", core.ObjectPerson, "P1", core.DirIn, day1)
	f.gate("RCPT-NIGHT-OUT", "Z1", core.ObjectPerson, "P1", core.DirOut, day2)

	for _, ts := range []int64{day1, day2} {
		sh, err := f.svc.ShiftFor(ts)
		if err != nil {
			t.Fatal(err)
		}
		if sh == nil || sh.ID != "shift-night" {
			t.Fatalf("时刻 %d 应归属 shift-night，实际 %+v", ts, sh)
		}
	}
	// 班次窗口外不属于任何班次。
	if sh, err := f.svc.ShiftFor(time.Date(2026, 9, 19, 7, 0, 0, 0, time.UTC).Unix()); err != nil || sh != nil {
		t.Fatalf("07:00 不应归属任何班次: %+v %v", sh, err)
	}

	shift, events, err := f.svc.ShiftReceipts("shift-night")
	if err != nil {
		t.Fatal(err)
	}
	if shift.ID != "shift-night" || len(events) != 2 {
		t.Fatalf("跨午夜班次的回执应包含午夜前后两条，实际 %d", len(events))
	}

	// 次日 00:45 的确认仍归属夜班。
	f.now.Store(time.Date(2026, 9, 19, 0, 45, 0, 0, time.UTC).Unix())
	f.evacuate("Z1") // 其余对象撤离（P1 已净离场）
	cc, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", "跨午夜确认")
	if err != nil {
		t.Fatalf("跨午夜确认失败: %v", err)
	}
	if cc.ShiftID != "shift-night" {
		t.Fatalf("00:45 的确认应归属 shift-night，实际 %q", cc.ShiftID)
	}
}

// ---------------------------------------------------------------------------
// 场景四：进程恢复后未决清场事项仍在
// ---------------------------------------------------------------------------

func TestProcessRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.db")

	// 第一个进程：P1 已进入未离开，V1 无任何回执。
	f1 := newFixtureAt(t, path)
	f1.setupStandard()
	f1.gate("RCPT-REC-IN", "Z1", core.ObjectPerson, "P1", core.DirIn, t0-1800)
	st1, err := f1.svc.ZoneStatus("Z1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st1.PendingObjects) != 2 {
		t.Fatalf("恢复前应有 2 个未撤离对象，实际 %d", len(st1.PendingObjects))
	}
	if freezeReasons(st1.OpenFreezes)[core.FreezeMissingReceipt] != 1 {
		t.Fatalf("恢复前应有 1 条缺回执冻结: %+v", st1.OpenFreezes)
	}
	freezeID := st1.OpenFreezes[0].ID
	f1.close()

	// 第二个进程：同一数据库文件重启，未决事项必须完整恢复。
	f2 := newFixtureAt(t, path)
	defer f2.close()
	st2, err := f2.svc.ZoneStatus("Z1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.PendingObjects) != 2 {
		t.Fatalf("恢复后应有 2 个未撤离对象，实际 %d", len(st2.PendingObjects))
	}
	if freezeReasons(st2.OpenFreezes)[core.FreezeMissingReceipt] != 1 {
		t.Fatalf("恢复后缺回执冻结应仍存在: %+v", st2.OpenFreezes)
	}
	if st2.OpenFreezes[0].ID != freezeID {
		t.Fatalf("冻结记录应在重启后保持同一 ID: %d != %d", st2.OpenFreezes[0].ID, freezeID)
	}
	// 重复计算不产生重复冻结。
	if st3, err := f2.svc.ZoneStatus("Z1"); err != nil || len(st3.OpenFreezes) != 1 {
		t.Fatalf("冻结同步应幂等: %+v %v", st3.OpenFreezes, err)
	}

	// 恢复后流程可继续推进：补齐回执、解除冻结、确认、核对。
	f2.gate("RCPT-REC-OUT", "Z1", core.ObjectPerson, "P1", core.DirOut, t0-300)
	f2.gate(nextReceipt(), "Z1", core.ObjectVehicle, "V1", core.DirIn, t0-1700)
	f2.gate(nextReceipt(), "Z1", core.ObjectVehicle, "V1", core.DirOut, t0-200)
	if err := f2.svc.ResolveFreeze(f2.actor("gc"), freezeID, "V1 回执已补齐"); err != nil {
		t.Fatalf("解除冻结: %v", err)
	}
	if _, err := f2.svc.ConfirmCrew(f2.actor("lead1"), "Z1", "c1", ""); err != nil {
		t.Fatalf("恢复后确认: %v", err)
	}
	if _, err := f2.svc.VerifyZone(f2.actor("gc"), "Z1"); err != nil {
		t.Fatalf("恢复后核对: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 冻结规则
// ---------------------------------------------------------------------------

func TestMissingReceiptFreezesAndBlocks(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	// V1 名册在列但无任何回执 → 班组不得确认。
	_, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", "")
	mustConflict(t, err, "缺回执时确认")

	st, err := f.svc.ZoneStatus("Z1")
	if err != nil {
		t.Fatal(err)
	}
	if freezeReasons(st.OpenFreezes)[core.FreezeMissingReceipt] != 2 { // P1、V1 均无回执
		t.Fatalf("应有 2 条缺回执冻结: %+v", st.OpenFreezes)
	}
	for _, p := range st.PendingObjects {
		if p.MissingReceipt && p.Net != 0 {
			t.Fatalf("无回执对象的净数量应为 0: %+v", p)
		}
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); statusOf(err) != 409 {
		t.Fatalf("缺回执时核对应失败: %v", err)
	}

	// 补齐回执并解除冻结后方可确认。
	f.evacuate("Z1")
	for _, fr := range st.OpenFreezes {
		if err := f.svc.ResolveFreeze(f.actor("gc"), fr.ID, "回执已补齐"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", ""); err != nil {
		t.Fatalf("补齐后应可确认: %v", err)
	}
}

func TestReentryFreeze(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()
	f.evacuate("Z1")
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", ""); err != nil {
		t.Fatal(err)
	}

	// 确认后 P1 重新进入 → 自动冻结，对象回到未撤离清单。
	f.gate("RCPT-REENTRY", "Z1", core.ObjectPerson, "P1", core.DirIn, t0+60)
	st, err := f.svc.ZoneStatus("Z1")
	if err != nil {
		t.Fatal(err)
	}
	if freezeReasons(st.OpenFreezes)[core.FreezeReentry] != 1 {
		t.Fatalf("应有重新进入冻结: %+v", st.OpenFreezes)
	}
	if len(st.PendingObjects) != 1 || st.PendingObjects[0].ObjectID != "P1" {
		t.Fatalf("P1 应回到未撤离清单: %+v", st.PendingObjects)
	}
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", ""); statusOf(err) != 409 {
		t.Fatalf("冻结期间不得再次确认: %v", err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); statusOf(err) != 409 {
		t.Fatalf("冻结期间不得核对: %v", err)
	}

	// 人员再次离开、指挥解除冻结后恢复。
	f.gate(nextReceipt(), "Z1", core.ObjectPerson, "P1", core.DirOut, t0+600)
	if err := f.svc.ResolveFreeze(f.actor("gc"), st.OpenFreezes[0].ID, "P1 已再次撤离并核实"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); err != nil {
		t.Fatalf("解除后应可核对: %v", err)
	}
}

func TestBoundaryExpansionFreezes(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()
	f.evacuate("Z1")
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); err != nil {
		t.Fatal(err)
	}

	// 边界扩大 → 新版本 + 冻结，旧确认与核对结论失效。
	v2, err := f.svc.ExpandZoneBoundary(f.actor("gc"), "Z1", "Z1 扩大后边界", true)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || !v2.Expanded {
		t.Fatalf("版本推进异常: %+v", v2)
	}
	st, err := f.svc.ZoneStatus("Z1")
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentVersion != 2 || st.Verified {
		t.Fatalf("v2 不应继承 v1 的核对结论: %+v", st)
	}
	if len(st.Confirmations) != 0 {
		t.Fatalf("v2 不应继承 v1 的确认: %+v", st.Confirmations)
	}
	if freezeReasons(st.OpenFreezes)[core.FreezeBoundaryExpanded] != 1 {
		t.Fatalf("应有边界扩大冻结: %+v", st.OpenFreezes)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); statusOf(err) != 409 {
		t.Fatalf("冻结期间不得核对: %v", err)
	}

	// 重新清场：解除冻结 → 重新确认 → 重新核对。
	if err := f.svc.ResolveFreeze(f.actor("gc"), st.OpenFreezes[0].ID, "扩大区域已排查"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", "v2 重新确认"); err != nil {
		t.Fatalf("v2 重新确认: %v", err)
	}
	zv, err := f.svc.VerifyZone(f.actor("gc"), "Z1")
	if err != nil {
		t.Fatalf("v2 核对: %v", err)
	}
	if zv.ZoneVersion != 2 {
		t.Fatalf("核对应对应 v2: %+v", zv)
	}
}

func TestZoneDependencyOrder(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	// Z2 依赖 Z1：Z1 未清场时 Z2 不得核对。
	f.evacuate("Z2")
	if _, err := f.svc.ConfirmCrew(f.actor("lead2"), "Z2", "c2", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z2"); statusOf(err) != 409 {
		t.Fatalf("依赖区未清场时不得核对: %v", err)
	}

	f.evacuate("Z1")
	if _, err := f.svc.ConfirmCrew(f.actor("lead1"), "Z1", "c1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z2"); err != nil {
		t.Fatalf("依赖区清场后应可核对: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 里程碑与放行
// ---------------------------------------------------------------------------

func TestMilestoneSealedAppendOnly(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	m, err := f.svc.SealMilestone(f.actor("gc"), "M1")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Sealed || m.SealedAt == nil {
		t.Fatalf("里程碑应已封存: %+v", m)
	}
	// 重复封存幂等，封存时间不变。
	m2, err := f.svc.SealMilestone(f.actor("gc"), "M1")
	if err != nil {
		t.Fatal(err)
	}
	if *m2.SealedAt != *m.SealedAt {
		t.Fatalf("重复封存不得改变封存时间: %d != %d", *m2.SealedAt, *m.SealedAt)
	}
	// 封存后只能追加偏差说明。
	if _, err := f.svc.AddDeviation(f.actor("lead1"), "M1", "转运就位比计划晚 12 分钟"); err != nil {
		t.Fatalf("封存后应可追加偏差说明: %v", err)
	}
	devs, err := f.svc.ListDeviations("M1")
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 {
		t.Fatalf("偏差说明应只增不改，实际 %d 条", len(devs))
	}
	// 班组负责人不得封存。
	if _, err := f.svc.SealMilestone(f.actor("lead1"), "M1"); statusOf(err) != 403 {
		t.Fatalf("班组负责人不得封存里程碑: %v", err)
	}
}

func TestReleaseFlowAndValidity(t *testing.T) {
	f := newFixture(t)
	defer f.close()
	f.setupStandard()

	// 未清场时不得发布放行。
	if _, err := f.svc.IssueRelease(f.actor("gc"), "勤务区撤离完毕"); statusOf(err) != 409 {
		t.Fatalf("未清场时不得放行: %v", err)
	}

	for _, z := range []struct{ zone, lead, crew string }{{"Z1", "lead1", "c1"}, {"Z2", "lead2", "c2"}} {
		f.evacuate(z.zone)
		if _, err := f.svc.ConfirmCrew(f.actor(z.lead), z.zone, z.crew, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.VerifyZone(f.actor("gc"), z.zone); err != nil {
			t.Fatal(err)
		}
	}
	// 核对与放行之间存在时间差：有效性判断不得依赖“核对时间 ≥ 放行时间”。
	f.now.Store(t0 + 3600)
	rel, err := f.svc.IssueRelease(f.actor("gc"), "勤务区撤离完毕，同意切换倒计时管制")
	if err != nil {
		t.Fatalf("全部清场后应可放行: %v", err)
	}
	latest, err := f.svc.LatestRelease()
	if err != nil || latest == nil {
		t.Fatalf("应存在放行结论: %v", err)
	}
	if !latest.Valid {
		t.Fatalf("刚发布的放行应有效: %+v", latest.Reason)
	}

	// 放行后发生重新进入 → 结论自动失效。
	f.gate("RCPT-POST-RELEASE", "Z1", core.ObjectPerson, "P1", core.DirIn, t0+1200)
	latest, err = f.svc.LatestRelease()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Valid {
		t.Fatal("重新进入后放行结论应失效")
	}
	if len(latest.Reason) == 0 {
		t.Fatal("失效应给出原因")
	}
	// 冻结未解除时不得再次放行。
	if _, err := f.svc.IssueRelease(f.actor("gc"), "强行放行"); statusOf(err) != 409 {
		t.Fatalf("冻结期间不得再次放行: %v", err)
	}

	// 排除原因并重新核对后，原结论仍因核对时间变化而失效，须重新发布。
	f.gate(nextReceipt(), "Z1", core.ObjectPerson, "P1", core.DirOut, t0+1800)
	freezes, err := f.svc.ListFreezes(true)
	if err != nil || len(freezes) != 1 {
		t.Fatalf("应有一条未解除冻结: %v", freezes)
	}
	if err := f.svc.ResolveFreeze(f.actor("gc"), freezes[0].ID, "P1 已再次撤离"); err != nil {
		t.Fatal(err)
	}
	f.now.Store(t0 + 7200)
	if _, err := f.svc.VerifyZone(f.actor("gc"), "Z1"); err != nil {
		t.Fatal(err)
	}
	latest, err = f.svc.LatestRelease()
	if err != nil {
		t.Fatal(err)
	}
	if latest.Valid {
		t.Fatal("重新核对后原放行结论应失效")
	}
	if _, err := f.svc.IssueRelease(f.actor("gc"), "复核完毕，重新放行"); err != nil {
		t.Fatalf("重新放行: %v", err)
	}
	latest, err = f.svc.LatestRelease()
	if err != nil || !latest.Valid {
		t.Fatalf("新放行结论应有效: %+v %v", latest, err)
	}
	_ = rel
}
