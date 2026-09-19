package service_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"evac/internal/domain"
	"evac/internal/service"
	"evac/internal/store"
)

var t0 = time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)

// newSvc 在临时目录创建服务，时钟固定为 now。
func newSvc(t *testing.T, dbPath string, now time.Time) (*service.Service, *store.Store) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := service.New(st)
	svc.Now = func() time.Time { return now }
	return svc, st
}

// must 解包 (T, error)；失败时 panic，测试框架将其计为失败。
// 注意 Go 不允许 f(t, g()) 形式的多返回值展开，故 must 不接收 t。
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func expectErrCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %q, got nil", code)
	}
	se, ok := err.(*service.Error)
	if !ok {
		t.Fatalf("expected *service.Error, got %T: %v", err, err)
	}
	if se.Code != code {
		t.Fatalf("expected code %q, got %q (%s)", code, se.Code, se.Message)
	}
}

// seedZone 建立危险区 Z1(v1)、责任单位 U1/U2、班组 C1(lead1)/C2(lead2)。
func seedZone(t *testing.T, svc *service.Service) {
	t.Helper()
	mkZone(t, svc, "Z1", "加注作业区")
	mkVersion(t, svc, "Z1", "半径300m", false)
	if err := svc.RegisterUnit("U1", "加注承包一队"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterUnit("U2", "消防保障分队"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []domain.Crew{
		{ID: "C1", Name: "加注一班", UnitID: "U1", ZoneID: "Z1", LeadUserID: "lead1"},
		{ID: "C2", Name: "消防一班", UnitID: "U2", ZoneID: "Z1", LeadUserID: "lead2"},
	} {
		if err := svc.RegisterCrew(c); err != nil {
			t.Fatal(err)
		}
	}
}

func addPerson(t *testing.T, svc *service.Service, zoneID, id, unitID, crewID, start, end string) {
	t.Helper()
	err := svc.RegisterPerson(domain.Person{
		ID: id, Name: "人员" + id, Post: "操作手", UnitID: unitID, CrewID: crewID,
		ZoneID: zoneID, ShiftStart: start, ShiftEnd: end,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exitScan(t *testing.T, svc *service.Service, scanID, kind, id string, at time.Time) {
	t.Helper()
	_, created, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: scanID, SubjectKind: kind, SubjectID: id,
		Gate: "GATE-1", Direction: domain.DirExit, ScannedAt: at,
	})
	if err != nil {
		t.Fatalf("exit scan %s: %v", scanID, err)
	}
	if !created {
		t.Fatalf("exit scan %s: expected created=true", scanID)
	}
}

func lead(userID string) service.Principal {
	return service.Principal{UserID: userID, Role: domain.RoleCrewLead}
}

func mkZone(t *testing.T, svc *service.Service, id, name string) {
	t.Helper()
	if _, err := svc.CreateZone(id, name); err != nil {
		t.Fatal(err)
	}
}

func mkVersion(t *testing.T, svc *service.Service, zoneID, boundary string, expanded bool) domain.ZoneVersion {
	t.Helper()
	v, err := svc.PublishZoneVersion(zoneID, boundary, expanded)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ---------- 场景一：重复扫码 ----------

func TestDuplicateScanIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, st := newSvc(t, filepath.Join(dir, "dup.db"), t0)
	defer st.Close()
	seedZone(t, svc)
	addPerson(t, svc, "Z1", "P1", "U1", "C1", "08:00", "20:00")
	if err := svc.RegisterAsset(domain.Asset{ID: "A1", Label: "活动门架", Kind: "gantry", UnitID: "U1", ZoneID: "Z1"}); err != nil {
		t.Fatal(err)
	}

	// 第一次离场扫码
	r1, created1, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "SCAN-1", SubjectKind: "person", SubjectID: "P1",
		Gate: "GATE-1", Direction: domain.DirExit, ScannedAt: t0.Add(-time.Hour),
	})
	if err != nil || !created1 {
		t.Fatalf("first scan: created=%v err=%v", created1, err)
	}
	// 闸机重发同一 scan_id：幂等，不产生第二条回执
	r2, created2, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "SCAN-1", SubjectKind: "person", SubjectID: "P1",
		Gate: "GATE-1", Direction: domain.DirExit, ScannedAt: t0.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("duplicate scan: %v", err)
	}
	if created2 {
		t.Fatal("duplicate scan must not create a new receipt")
	}
	if r2.ID != r1.ID || r2.ScanID != "SCAN-1" {
		t.Fatalf("idempotent replay returned different receipt: %+v vs %+v", r2, r1)
	}
	if n := must(st.CountReceipts()); n != 1 {
		t.Fatalf("expected 1 receipt, got %d", n)
	}

	// P1 重新进入 → 冻结；重复上报该进入扫码不得产生第二条冻结
	if _, _, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "SCAN-2", SubjectKind: "person", SubjectID: "P1",
		Gate: "GATE-1", Direction: domain.DirEntry, ScannedAt: t0.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "SCAN-2", SubjectKind: "person", SubjectID: "P1",
		Gate: "GATE-1", Direction: domain.DirEntry, ScannedAt: t0.Add(-30 * time.Minute),
	}); err != nil || created {
		t.Fatalf("duplicate entry scan: created=%v err=%v", created, err)
	}
	cl := must(svc.Evaluate("Z1"))
	if len(cl.ActiveFreezes) != 1 || cl.ActiveFreezes[0].Reason != domain.FreezeReEntry {
		t.Fatalf("expected exactly 1 re_entry freeze, got %+v", cl.ActiveFreezes)
	}
	if len(cl.Pending) != 2 { // P1 re_entered + A1 missing_receipt
		t.Fatalf("expected 2 pending objects, got %+v", cl.Pending)
	}
	var p1 *service.PendingObject
	for i := range cl.Pending {
		if cl.Pending[i].ID == "P1" {
			p1 = &cl.Pending[i]
		}
	}
	if p1 == nil || p1.Reason != "re_entered" || p1.UnitName != "加注承包一队" {
		t.Fatalf("P1 pending state wrong: %+v", p1)
	}
	if n := must(st.CountReceipts()); n != 2 {
		t.Fatalf("expected 2 receipts, got %d", n)
	}
}

// ---------- 场景二：并发确认 ----------

func TestConcurrentConfirmationsAndScans(t *testing.T) {
	dir := t.TempDir()
	svc, st := newSvc(t, filepath.Join(dir, "conc.db"), t0)
	defer st.Close()
	mkZone(t, svc, "ZC", "并发测试区")
	mkVersion(t, svc, "ZC", "半径100m", false)
	if err := svc.RegisterUnit("U1", "单位一"); err != nil {
		t.Fatal(err)
	}
	const crews = 5
	const perCrew = 4
	for i := 0; i < crews; i++ {
		crewID := fmt.Sprintf("C%d", i)
		if err := svc.RegisterCrew(domain.Crew{
			ID: crewID, Name: "班组" + crewID, UnitID: "U1", ZoneID: "ZC", LeadUserID: "lead-" + crewID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 5 个班组 × 4 个并发确认请求（含同组重复确认）
	var wg sync.WaitGroup
	errs := make(chan error, crews*perCrew)
	for i := 0; i < crews; i++ {
		crewID := fmt.Sprintf("C%d", i)
		for j := 0; j < perCrew; j++ {
			wg.Add(1)
			go func(cid string) {
				defer wg.Done()
				_, _, err := svc.ConfirmCrew(cid, lead("lead-"+cid))
				errs <- err
			}(crewID)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent confirm error: %v", err)
		}
	}
	if n := must(st.CountConfirmations()); n != crews {
		t.Fatalf("expected %d confirmations (one per crew), got %d", crews, n)
	}
	cl := must(svc.Evaluate("ZC"))
	for _, c := range cl.Crews {
		if !c.Confirmed {
			t.Fatalf("crew %s not confirmed after concurrent confirms", c.CrewID)
		}
	}

	// 并发上报同一 scan_id：只落一条回执
	addPerson(t, svc, "ZC", "PX", "U1", "C0", "00:00", "00:00")
	const scans = 12
	createdCnt := make(chan bool, scans)
	errs2 := make(chan error, scans)
	for i := 0; i < scans; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := svc.RecordGateReceipt(service.ReceiptInput{
				ScanID: "SCAN-DUP", SubjectKind: "person", SubjectID: "PX",
				Gate: "GATE-9", Direction: domain.DirExit, ScannedAt: t0,
			})
			errs2 <- err
			createdCnt <- created
		}()
	}
	wg.Wait()
	close(errs2)
	close(createdCnt)
	trues := 0
	for err := range errs2 {
		if err != nil {
			t.Fatalf("concurrent scan error: %v", err)
		}
	}
	for c := range createdCnt {
		if c {
			trues++
		}
	}
	if trues != 1 {
		t.Fatalf("expected exactly 1 created receipt among %d concurrent scans, got %d", scans, trues)
	}
	if n := must(st.CountReceipts()); n != 1 {
		t.Fatalf("expected 1 receipt total, got %d", n)
	}

	// 并发核对同一区域：放行结论幂等，只生成一条
	// 先清空未决：PX 已离场，无其他名册对象；班组均已确认。
	const verifies = 6
	relIDs := make(chan string, verifies)
	errs3 := make(chan error, verifies)
	for i := 0; i < verifies; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			rel, _, err := svc.VerifyZone("ZC", fmt.Sprintf("cmd-%d", k))
			if err != nil {
				errs3 <- err
				return
			}
			relIDs <- rel.Conclusion
			errs3 <- nil
		}(i)
	}
	wg.Wait()
	close(errs3)
	close(relIDs)
	for err := range errs3 {
		if err != nil {
			t.Fatalf("concurrent verify error: %v", err)
		}
	}
	seen := map[string]int{}
	for c := range relIDs {
		seen[c]++
	}
	if len(seen) != 1 {
		t.Fatalf("expected a single identical release conclusion, got %v", seen)
	}
	releases := must(svc.ListReleases())
	if len(releases) != 1 {
		t.Fatalf("expected exactly 1 release, got %d", len(releases))
	}
}

// ---------- 场景三：跨午夜班次 ----------

func TestCrossMidnightShift(t *testing.T) {
	// 纯函数校验
	at := func(h, m int) time.Time { return time.Date(2026, 9, 20, h, m, 0, 0, time.UTC) }
	cases := []struct {
		start, end string
		t          time.Time
		want       bool
	}{
		{"22:00", "06:00", at(23, 30), true}, // 跨午夜班，当夜 23:30
		{"22:00", "06:00", at(1, 30), true},  // 跨午夜班，次日 01:30
		{"22:00", "06:00", at(5, 59), true},  // 跨午夜班，班次末尾
		{"22:00", "06:00", at(6, 0), false},  // 跨午夜班，已下勤
		{"22:00", "06:00", at(12, 0), false}, // 跨午夜班，白天不在勤
		{"08:00", "20:00", at(9, 0), true},   // 白班在勤
		{"08:00", "20:00", at(1, 30), false}, // 白班凌晨不在勤
		{"00:00", "00:00", at(3, 0), true},   // 24 小时值守
	}
	for _, c := range cases {
		if got := domain.WithinShift(c.start, c.end, c.t); got != c.want {
			t.Errorf("WithinShift(%s,%s,%v)=%v want %v", c.start, c.end, c.t, got, c.want)
		}
	}

	// 业务校验：凌晨 01:30，跨午夜班人员离场扫码应被正确计入清场
	afterMidnight := time.Date(2026, 9, 20, 1, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	svc, st := newSvc(t, filepath.Join(dir, "night.db"), afterMidnight)
	defer st.Close()
	seedZone(t, svc)
	addPerson(t, svc, "Z1", "P-NIGHT", "U1", "C1", "22:00", "06:00")
	addPerson(t, svc, "Z1", "P-DAY", "U1", "C1", "08:00", "20:00")

	cl := must(svc.Evaluate("Z1"))
	onShift := map[string]bool{}
	for _, p := range cl.Pending {
		onShift[p.ID] = p.OnShift
	}
	if !onShift["P-NIGHT"] {
		t.Error("P-NIGHT should be on shift at 01:30 (22:00-06:00 cross-midnight)")
	}
	if onShift["P-DAY"] {
		t.Error("P-DAY should not be on shift at 01:30 (08:00-20:00)")
	}

	// 跨午夜班人员 01:30 扫码离场 → 立即视为已撤离
	exitScan(t, svc, "SCAN-N1", "person", "P-NIGHT", afterMidnight)
	cl = must(svc.Evaluate("Z1"))
	for _, p := range cl.Pending {
		if p.ID == "P-NIGHT" {
			t.Fatalf("P-NIGHT should be cleared after 01:30 exit scan, still pending: %+v", p)
		}
	}
	if len(cl.Pending) != 1 || cl.Pending[0].ID != "P-DAY" {
		t.Fatalf("only P-DAY should remain pending, got %+v", cl.Pending)
	}
}

// ---------- 场景四：进程恢复 ----------

func TestRecoveryAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "recover.db")

	// 第一次“进程”：登记并留下未决事项
	func() {
		svc, st := newSvc(t, dbPath, t0)
		defer st.Close()
		seedZone(t, svc)
		addPerson(t, svc, "Z1", "P1", "U1", "C1", "08:00", "20:00")
		addPerson(t, svc, "Z1", "P2", "U1", "C1", "08:00", "20:00")
		addPerson(t, svc, "Z1", "P3", "U2", "C2", "08:00", "20:00")
		exitScan(t, svc, "SCAN-R1", "person", "P1", t0.Add(-2*time.Hour))
		// P3 离场后重新进入 → 冻结
		exitScan(t, svc, "SCAN-R2", "person", "P3", t0.Add(-2*time.Hour))
		if _, _, err := svc.RecordGateReceipt(service.ReceiptInput{
			ScanID: "SCAN-R3", SubjectKind: "person", SubjectID: "P3",
			Gate: "GATE-1", Direction: domain.DirEntry, ScannedAt: t0.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		// C1 班组已确认；P2 无任何回执（未决）
		if _, _, err := svc.ConfirmCrew("C1", lead("lead1")); err != nil {
			t.Fatal(err)
		}
	}()

	// 第二次“进程”：同一数据库文件恢复，未决清场事项必须完整保留
	svc, st := newSvc(t, dbPath, t0)
	defer st.Close()
	cl := must(svc.Evaluate("Z1"))
	if cl.Status != domain.StatusFrozen {
		t.Fatalf("expected frozen after restart, got %s", cl.Status)
	}
	if len(cl.ActiveFreezes) != 1 || cl.ActiveFreezes[0].Reason != domain.FreezeReEntry {
		t.Fatalf("re-entry freeze lost after restart: %+v", cl.ActiveFreezes)
	}
	pending := map[string]string{}
	for _, p := range cl.Pending {
		pending[p.ID] = p.Reason
	}
	if pending["P2"] != "missing_receipt" {
		t.Fatalf("P2 missing_receipt pending lost after restart: %v", pending)
	}
	if pending["P3"] != "re_entered" {
		t.Fatalf("P3 re_entered pending lost after restart: %v", pending)
	}
	if _, ok := pending["P1"]; ok {
		t.Fatal("P1 exited before restart, must not be pending")
	}
	if cl.LastConfirmationAt == nil {
		t.Fatal("C1 confirmation lost after restart")
	}

	// 恢复后继续推进：P3 再次离场 → 解冻 → P2 离场 → 确认 C2 → 放行
	exitScan(t, svc, "SCAN-R4", "person", "P3", t0.Add(-30*time.Minute))
	if err := svc.ResolveFreeze("Z1", cl.ActiveFreezes[0].ID, "ground-cmd"); err != nil {
		t.Fatalf("resolve freeze after restart: %v", err)
	}
	exitScan(t, svc, "SCAN-R5", "person", "P2", t0.Add(-10*time.Minute))
	if _, _, err := svc.ConfirmCrew("C2", lead("lead2")); err != nil {
		t.Fatal(err)
	}
	rel, cl2, err := svc.VerifyZone("Z1", "ground-cmd")
	if err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
	if cl2.Status != domain.StatusReleased || rel.ZoneVersion != 1 {
		t.Fatalf("expected released v1, got %+v", cl2.Status)
	}
}

// ---------- 完整放行流程：缺回执冻结、依赖、里程碑封存、边界扩大 ----------

func TestFullReleaseFlow(t *testing.T) {
	dir := t.TempDir()
	svc, st := newSvc(t, filepath.Join(dir, "flow.db"), t0)
	defer st.Close()
	seedZone(t, svc)
	// Z2 依赖 Z1，Z2 无名册对象
	mkZone(t, svc, "Z2", "发射工位区")
	mkVersion(t, svc, "Z2", "半径500m", false)
	if err := svc.AddDependency("Z2", "Z1"); err != nil {
		t.Fatal(err)
	}
	addPerson(t, svc, "Z1", "P1", "U1", "C1", "08:00", "20:00")
	addPerson(t, svc, "Z1", "P2", "U2", "C2", "08:00", "20:00")
	if err := svc.RegisterAsset(domain.Asset{ID: "A1", Label: "槽车", Kind: "vehicle", UnitID: "U1", ZoneID: "Z1"}); err != nil {
		t.Fatal(err)
	}

	// 1) 缺回执 → 核对时自动冻结
	_, _, err := svc.VerifyZone("Z1", "ground-cmd")
	expectErrCode(t, err, "zone_frozen")
	cl := must(svc.Evaluate("Z1"))
	if !hasFreeze(cl.ActiveFreezes, domain.FreezeMissingReceipt) {
		t.Fatalf("expected missing_receipt freeze, got %+v", cl.ActiveFreezes)
	}
	// 冻结未解除前，即使补齐回执也不能放行
	exitScan(t, svc, "S1", "person", "P1", t0.Add(-time.Hour))
	exitScan(t, svc, "S2", "person", "P2", t0.Add(-time.Hour))
	exitScan(t, svc, "S3", "asset", "A1", t0.Add(-time.Hour))
	_, _, err = svc.VerifyZone("Z1", "ground-cmd")
	expectErrCode(t, err, "zone_frozen")
	// 解除冻结 → 班组未确认仍不能放行
	if err := svc.ResolveFreeze("Z1", cl.ActiveFreezes[0].ID, "ground-cmd"); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.VerifyZone("Z1", "ground-cmd")
	expectErrCode(t, err, "crew_unconfirmed")
	if _, _, err := svc.ConfirmCrew("C1", lead("lead1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ConfirmCrew("C2", lead("lead2")); err != nil {
		t.Fatal(err)
	}
	// 2) 放行 Z1，里程碑自动封存
	rel, cl, err := svc.VerifyZone("Z1", "ground-cmd")
	if err != nil {
		t.Fatalf("verify Z1: %v", err)
	}
	if cl.Status != domain.StatusReleased || rel.ZoneVersion != 1 {
		t.Fatalf("Z1 should be released at v1: %+v", cl.Status)
	}
	// 3) Z2 依赖未满足时不能放行（Z1 放行前 Z2 已被阻塞——此处验证放行后依赖解除）
	mkZone(t, svc, "Z3", "依赖未满足区")
	mkVersion(t, svc, "Z3", "半径50m", false)
	if err := svc.AddDependency("Z3", "Z2"); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.VerifyZone("Z3", "ground-cmd")
	expectErrCode(t, err, "dependency_blocked")
	if _, _, err := svc.VerifyZone("Z2", "ground-cmd"); err != nil {
		t.Fatalf("verify Z2: %v", err)
	}
	if _, _, err := svc.VerifyZone("Z3", "ground-cmd"); err != nil {
		t.Fatalf("verify Z3 after dependency released: %v", err)
	}
	// 4) 已封存里程碑：只能追加偏差说明
	if _, err := svc.UpdateMilestone("MS-REL-Z1-v1", "改名", "篡改结论"); err == nil {
		t.Fatal("sealed milestone must reject updates")
	} else {
		expectErrCode(t, err, "milestone_sealed")
	}
	dev, err := svc.AppendDeviation("MS-REL-Z1-v1", "复核时补充：P2 离场回执延迟 2 分钟到达", "ground-cmd")
	if err != nil {
		t.Fatalf("append deviation: %v", err)
	}
	if dev.ID == 0 {
		t.Fatal("deviation should have an ID")
	}
	rv := must(svc.GetRelease("Z1"))
	if len(rv.Deviations) != 1 {
		t.Fatalf("release view should include 1 deviation, got %+v", rv.Deviations)
	}

	// 5) 边界扩大 → 新版本 + 冻结，旧确认失效，须重新清场确认
	v2 := mkVersion(t, svc, "Z1", "半径400m", true)
	if v2.Version != 2 {
		t.Fatalf("expected v2, got %d", v2.Version)
	}
	cl = must(svc.Evaluate("Z1"))
	if cl.Status != domain.StatusFrozen || !hasFreeze(cl.ActiveFreezes, domain.FreezeBoundaryExpanded) {
		t.Fatalf("expected boundary_expanded freeze, got %+v / %+v", cl.Status, cl.ActiveFreezes)
	}
	for _, c := range cl.Crews {
		if c.Confirmed {
			t.Fatalf("confirmations must reset on new version, crew %s still confirmed", c.CrewID)
		}
	}
	// 人员重新进入 → 追加 re_entry 冻结
	if _, _, err := svc.RecordGateReceipt(service.ReceiptInput{
		ScanID: "S4", SubjectKind: "person", SubjectID: "P1",
		Gate: "GATE-2", Direction: domain.DirEntry, ScannedAt: t0.Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	cl = must(svc.Evaluate("Z1"))
	if !hasFreeze(cl.ActiveFreezes, domain.FreezeReEntry) {
		t.Fatalf("expected re_entry freeze, got %+v", cl.ActiveFreezes)
	}
	// 重新进入者未再次离场前，冻结不可解除
	for _, f := range cl.ActiveFreezes {
		if f.Reason == domain.FreezeReEntry {
			expectErrCode(t, svc.ResolveFreeze("Z1", f.ID, "ground-cmd"), "freeze_condition_active")
		}
	}
	// P1 再次离场 → 解除 re_entry；重新确认两个班组 → 解除边界扩大冻结 → 放行 v2
	exitScan(t, svc, "S5", "person", "P1", t0.Add(20*time.Minute))
	cl = must(svc.Evaluate("Z1"))
	for _, f := range cl.ActiveFreezes {
		if f.Reason == domain.FreezeReEntry {
			if err := svc.ResolveFreeze("Z1", f.ID, "ground-cmd"); err != nil {
				t.Fatalf("resolve re_entry: %v", err)
			}
		}
	}
	if _, _, err := svc.ConfirmCrew("C1", lead("lead1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ConfirmCrew("C2", lead("lead2")); err != nil {
		t.Fatal(err)
	}
	cl = must(svc.Evaluate("Z1"))
	for _, f := range cl.ActiveFreezes {
		if f.Reason == domain.FreezeBoundaryExpanded {
			if err := svc.ResolveFreeze("Z1", f.ID, "ground-cmd"); err != nil {
				t.Fatalf("resolve boundary_expanded: %v", err)
			}
		}
	}
	rel2, cl, err := svc.VerifyZone("Z1", "ground-cmd")
	if err != nil {
		t.Fatalf("verify Z1 v2: %v", err)
	}
	if rel2.ZoneVersion != 2 || cl.Status != domain.StatusReleased {
		t.Fatalf("expected released v2, got %+v", cl.Status)
	}
	// v1 的放行结论与里程碑保持封存、互不影响
	rv1 := must(svc.GetRelease("Z1"))
	if rv1.Version != 2 {
		t.Fatalf("current release should be v2, got v%d", rv1.Version)
	}
}

func hasFreeze(freezes []domain.Freeze, reason domain.FreezeReason) bool {
	for _, f := range freezes {
		if f.Reason == reason {
			return true
		}
	}
	return false
}

// ---------- 通知与指挥席 ----------

func TestNotifyAndDashboard(t *testing.T) {
	dir := t.TempDir()
	svc, st := newSvc(t, filepath.Join(dir, "dash.db"), t0)
	defer st.Close()
	seedZone(t, svc)
	addPerson(t, svc, "Z1", "P1", "U1", "C1", "08:00", "20:00")
	addPerson(t, svc, "Z1", "P2", "U2", "C2", "08:00", "20:00")

	// 通知按责任单位生成，重复调用幂等
	ns := must(svc.NotifyZone("Z1"))
	if len(ns) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(ns))
	}
	ns2 := must(svc.NotifyZone("Z1"))
	if len(ns2) != 2 {
		t.Fatalf("notify must be idempotent, got %d", len(ns2))
	}
	unitOf := map[string]string{}
	for _, n := range ns2 {
		unitOf[n.SubjectID] = n.UnitID
	}
	if unitOf["P1"] != "U1" || unitOf["P2"] != "U2" {
		t.Fatalf("notifications routed to wrong units: %v", unitOf)
	}

	// 承包单位回执本单位通知
	var n1 int64
	for _, n := range ns2 {
		if n.UnitID == "U1" {
			n1 = n.ID
		}
	}
	acked := must(svc.AckNotification(n1, service.Principal{UserID: "boss", Role: domain.RoleContractor, UnitID: "U1"}))
	if acked.AckedAt == nil || acked.AckedBy != "boss" {
		t.Fatalf("ack not recorded: %+v", acked)
	}

	// 指挥席总览：最后确认时间、未撤离对象、冻结原因、通知回执
	if _, _, err := svc.ConfirmCrew("C1", lead("lead1")); err != nil {
		t.Fatal(err)
	}
	dash := must(svc.Dashboard())
	if len(dash) != 1 {
		t.Fatalf("expected 1 zone in dashboard, got %d", len(dash))
	}
	z := dash[0]
	if z.ZoneID != "Z1" || z.LastConfirmationAt == nil {
		t.Fatalf("dashboard missing last confirmation time: %+v", z)
	}
	if len(z.Pending) != 2 {
		t.Fatalf("dashboard should list 2 pending objects, got %+v", z.Pending)
	}
	if len(z.Notifications) != 2 {
		t.Fatalf("dashboard should list 2 notifications, got %+v", z.Notifications)
	}
	ackedCount := 0
	for _, n := range z.Notifications {
		if n.AckedAt != nil {
			ackedCount++
		}
	}
	if ackedCount != 1 {
		t.Fatalf("expected 1 acked notification on dashboard, got %d", ackedCount)
	}
}
