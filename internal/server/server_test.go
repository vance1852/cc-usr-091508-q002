package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v2"

	"evacsvc/internal/core"
	"evacsvc/internal/server"
)

// newApp 使用演示种子数据启动内存中的应用（每次独立数据库文件）。
func newApp(t *testing.T) *fiber.App {
	t.Helper()
	db, err := core.Open(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if err := core.Seed(db); err != nil {
		t.Fatalf("种子数据: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return server.New(core.NewService(db, nil))
}

func do(t *testing.T, app *fiber.App, method, path, actor string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set("X-Actor-Id", actor)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("响应解析失败: %v\n%s", err, data)
	}
	return v
}

// ---------------------------------------------------------------------------
// 认证与角色边界
// ---------------------------------------------------------------------------

func TestAuthRequired(t *testing.T) {
	app := newApp(t)
	if code, _ := do(t, app, "GET", "/api/v1/dashboard", "", nil); code != 401 {
		t.Fatalf("缺少身份头应 401，实际 %d", code)
	}
	if code, _ := do(t, app, "GET", "/api/v1/dashboard", "nobody", nil); code != 401 {
		t.Fatalf("未知操作者应 401，实际 %d", code)
	}
}

func TestLaunchCommanderReadOnly(t *testing.T) {
	app := newApp(t)
	// 发射指挥只能接收最终放行结论。
	if code, _ := do(t, app, "GET", "/api/v1/releases", "lc-01", nil); code != 200 {
		t.Fatalf("发射指挥应能读取放行结论，实际 %d", code)
	}
	if code, _ := do(t, app, "GET", "/api/v1/releases/latest", "lc-01", nil); code != 404 {
		t.Fatalf("尚无结论时应 404，实际 %d", code)
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/dashboard"},
		{"GET", "/api/v1/zones"},
		{"GET", "/api/v1/personnel"},
		{"GET", "/api/v1/freezes"},
		{"GET", "/api/v1/milestones"},
		{"POST", "/api/v1/releases"},
		{"POST", "/api/v1/gate-events"},
		{"POST", "/api/v1/zones/Z-FUEL/verify"},
	} {
		if code, _ := do(t, app, tc.method, tc.path, "lc-01", nil); code != 403 {
			t.Fatalf("发射指挥 %s %s 应 403，实际 %d", tc.method, tc.path, code)
		}
	}
}

func TestContractorIsolation(t *testing.T) {
	app := newApp(t)

	// 承包单位只能看到本单位人员。
	_, data := do(t, app, "GET", "/api/v1/personnel", "ctr-alpha", nil)
	alpha := decode[[]core.Person](t, data)
	if len(alpha) != 2 {
		t.Fatalf("甲安公司应只有 2 名人员，实际 %d", len(alpha))
	}
	for _, p := range alpha {
		if p.UnitID != "unit-alpha" {
			t.Fatalf("承包单位看到了其他单位人员: %+v", p)
		}
	}
	_, data = do(t, app, "GET", "/api/v1/personnel", "ctr-beta", nil)
	beta := decode[[]core.Person](t, data)
	if len(beta) != 3 {
		t.Fatalf("乙联公司应只有 3 名人员，实际 %d", len(beta))
	}
	// 车辆设备同样隔离。
	_, data = do(t, app, "GET", "/api/v1/vehicles", "ctr-alpha", nil)
	if vehicles := decode[[]core.Vehicle](t, data); len(vehicles) != 1 || vehicles[0].UnitID != "unit-alpha" {
		t.Fatalf("车辆隔离失败: %+v", vehicles)
	}
	// 地面保障指挥可见全部。
	_, data = do(t, app, "GET", "/api/v1/personnel", "gc-01", nil)
	if all := decode[[]core.Person](t, data); len(all) != 5 {
		t.Fatalf("指挥应见全部 5 名人员，实际 %d", len(all))
	}
	// 承包单位不得访问看板。
	if code, _ := do(t, app, "GET", "/api/v1/dashboard", "ctr-alpha", nil); code != 403 {
		t.Fatalf("承包单位不得查看指挥看板，实际 %d", code)
	}
}

func TestNotificationAckIsolation(t *testing.T) {
	app := newApp(t)
	// 地面保障指挥向甲安公司发出清场通知。
	code, data := do(t, app, "POST", "/api/v1/zones/Z-FUEL/notify", "gc-01",
		map[string]string{"unit_id": "unit-alpha", "subject": "加注区有未撤离对象", "body": "请立即核实"})
	if code != 201 {
		t.Fatalf("发送通知失败: %d %s", code, data)
	}
	n := decode[core.Notification](t, data)

	// 乙联公司看不到、也不得回执甲安公司的通知。
	_, data = do(t, app, "GET", "/api/v1/notifications", "ctr-beta", nil)
	if list := decode[[]core.Notification](t, data); len(list) != 0 {
		t.Fatalf("乙联不应看到甲安的通知: %+v", list)
	}
	if code, _ := do(t, app, "POST", fmt.Sprintf("/api/v1/notifications/%d/ack", n.ID), "ctr-beta", nil); code != 403 {
		t.Fatalf("跨单位回执应 403，实际 %d", code)
	}
	// 甲安公司回执，重复回执幂等。
	code, data = do(t, app, "POST", fmt.Sprintf("/api/v1/notifications/%d/ack", n.ID), "ctr-alpha", nil)
	if code != 200 {
		t.Fatalf("本单位回执应成功: %d %s", code, data)
	}
	acked := decode[core.Notification](t, data)
	if acked.AckedAt == nil {
		t.Fatal("回执时间应已记录")
	}
	code, data = do(t, app, "POST", fmt.Sprintf("/api/v1/notifications/%d/ack", n.ID), "ctr-alpha", nil)
	if code != 200 {
		t.Fatalf("重复回执应幂等: %d %s", code, data)
	}
}

func TestCrewLeadScope(t *testing.T) {
	app := newApp(t)
	// 班组负责人不得确认其他班组。
	if code, _ := do(t, app, "POST", "/api/v1/zones/Z-FUEL/crews/crew-fire/confirm", "lead-fuel", nil); code != 403 {
		t.Fatalf("跨班组确认应 403，实际 %d", code)
	}
	// 班组负责人只能看到本组人员。
	_, data := do(t, app, "GET", "/api/v1/personnel", "lead-fuel", nil)
	for _, p := range decode[[]core.Person](t, data) {
		if p.CrewID != "crew-fuel" {
			t.Fatalf("班组负责人看到了其他班组人员: %+v", p)
		}
	}
}

// ---------------------------------------------------------------------------
// 门禁回执幂等（HTTP 层）
// ---------------------------------------------------------------------------

func TestGateEventIdempotentHTTP(t *testing.T) {
	app := newApp(t)
	body := map[string]any{
		"receipt_no": "RCPT-HTTP-1", "gate_id": "GATE-3", "zone_id": "Z-FUEL",
		"object_type": "person", "object_id": "P-101", "direction": "in", "occurred_at": 1758240000,
	}
	code, data := do(t, app, "POST", "/api/v1/gate-events", "gate-sys", body)
	if code != 201 {
		t.Fatalf("首次录入应 201，实际 %d %s", code, data)
	}
	first := decode[core.GateEvent](t, data)
	code, data = do(t, app, "POST", "/api/v1/gate-events", "gate-sys", body)
	if code != 200 {
		t.Fatalf("重复扫码应 200，实际 %d %s", code, data)
	}
	second := decode[core.GateEvent](t, data)
	if !second.Duplicate || second.ID != first.ID {
		t.Fatalf("重复扫码应判重并返回同一记录: %+v", second)
	}
	// 非法方向。
	body["receipt_no"] = "RCPT-HTTP-2"
	body["direction"] = "sideways"
	if code, _ := do(t, app, "POST", "/api/v1/gate-events", "gate-sys", body); code != 400 {
		t.Fatalf("非法方向应 400，实际 %d", code)
	}
}

// ---------------------------------------------------------------------------
// 端到端：撤离 → 确认 → 核对 → 放行 → 发射指挥读取
// ---------------------------------------------------------------------------

func TestEndToEndReleaseFlow(t *testing.T) {
	app := newApp(t)
	ts := int64(1758240000)
	gate := func(receipt, zone, objType, objID, dir string) {
		t.Helper()
		code, data := do(t, app, "POST", "/api/v1/gate-events", "gate-sys", map[string]any{
			"receipt_no": receipt, "gate_id": "GATE-1", "zone_id": zone,
			"object_type": objType, "object_id": objID, "direction": dir, "occurred_at": ts,
		})
		if code != 201 {
			t.Fatalf("门禁回执 %s 失败: %d %s", receipt, code, data)
		}
		ts++
	}
	// 三个区域全部对象 进+出。
	for _, tc := range []struct{ zone, objType, objID string }{
		{"Z-FUEL", "person", "P-101"}, {"Z-FUEL", "person", "P-102"}, {"Z-FUEL", "vehicle", "V-201"},
		{"Z-PAD", "person", "P-103"}, {"Z-PAD", "vehicle", "V-202"},
		{"Z-ASSY", "person", "P-104"}, {"Z-ASSY", "person", "P-105"}, {"Z-ASSY", "vehicle", "G-301"},
	} {
		gate("R-"+tc.objID+"-IN", tc.zone, tc.objType, tc.objID, "in")
		gate("R-"+tc.objID+"-OUT", tc.zone, tc.objType, tc.objID, "out")
	}
	// 班组确认。
	for _, tc := range []struct{ zone, crew, lead string }{
		{"Z-FUEL", "crew-fuel", "lead-fuel"},
		{"Z-PAD", "crew-fire", "lead-fire"},
		{"Z-ASSY", "crew-measure", "lead-measure"},
	} {
		code, data := do(t, app, "POST", fmt.Sprintf("/api/v1/zones/%s/crews/%s/confirm", tc.zone, tc.crew), tc.lead, nil)
		if code != 201 {
			t.Fatalf("班组确认失败: %d %s", code, data)
		}
	}
	// 地面保障指挥核对（Z-PAD 依赖 Z-FUEL，顺序无关，系统按依赖校验）。
	for _, zone := range []string{"Z-FUEL", "Z-ASSY", "Z-PAD"} {
		code, data := do(t, app, "POST", "/api/v1/zones/"+zone+"/verify", "gc-01", nil)
		if code != 201 {
			t.Fatalf("区域 %s 核对失败: %d %s", zone, code, data)
		}
	}
	// 看板：各区域已清场、有最后确认时间。
	_, data := do(t, app, "GET", "/api/v1/dashboard", "gc-01", nil)
	dash := decode[[]core.ZoneStatus](t, data)
	if len(dash) != 3 {
		t.Fatalf("看板应含 3 个区域，实际 %d", len(dash))
	}
	for _, z := range dash {
		if !z.Verified || z.LastConfirmationAt == nil || len(z.PendingObjects) != 0 || len(z.OpenFreezes) != 0 {
			t.Fatalf("区域 %s 看板状态异常: %+v", z.ZoneID, z)
		}
	}
	// 发布放行，发射指挥读取最终结论。
	code, data := do(t, app, "POST", "/api/v1/releases", "gc-01", map[string]string{"conclusion": "勤务区撤离完毕"})
	if code != 201 {
		t.Fatalf("发布放行失败: %d %s", code, data)
	}
	code, data = do(t, app, "GET", "/api/v1/releases/latest", "lc-01", nil)
	if code != 200 {
		t.Fatalf("发射指挥读取放行结论失败: %d", code)
	}
	latest := decode[core.ReleaseView](t, data)
	if !latest.Valid {
		t.Fatalf("放行结论应有效: %+v", latest.Reason)
	}
}
