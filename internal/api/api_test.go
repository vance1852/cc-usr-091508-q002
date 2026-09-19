package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"evac/internal/api"
	"evac/internal/domain"
	"evac/internal/service"
	"evac/internal/store"

	"github.com/gofiber/fiber/v2"
)

var apiT0 = time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC)

func newTestApp(t *testing.T) (*fiber.App, *service.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := service.New(st)
	svc.Now = func() time.Time { return apiT0 }
	return api.NewApp(svc), svc
}

type hdr map[string]string

var (
	gcHead   = hdr{"X-User-Id": "ground-cmd", "X-Role": "ground_command"}
	lcHead   = hdr{"X-User-Id": "launch-cmd", "X-Role": "launch_command"}
	u1Head   = hdr{"X-User-Id": "u1-boss", "X-Role": "contractor", "X-Unit-Id": "U1"}
	u2Head   = hdr{"X-User-Id": "u2-boss", "X-Role": "contractor", "X-Unit-Id": "U2"}
	lead1Hdr = hdr{"X-User-Id": "lead1", "X-Role": "crew_lead", "X-Crew-Id": "C1"}
	lead2Hdr = hdr{"X-User-Id": "lead2", "X-Role": "crew_lead", "X-Crew-Id": "C2"}
)

func do(t *testing.T, app *fiber.App, method, path string, body any, h hdr) (int, map[string]any) {
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("%s %s: invalid json %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, out
}

// seedAll 通过 HTTP 以地面保障指挥身份登记完整现场数据。
func seedAll(t *testing.T, app *fiber.App) {
	t.Helper()
	post := func(path string, body any, want int) {
		t.Helper()
		code, resp := do(t, app, http.MethodPost, path, body, gcHead)
		if code != want {
			t.Fatalf("POST %s: got %d want %d: %v", path, code, want, resp)
		}
	}
	post("/api/zones", fiber.Map{"id": "Z1", "name": "加注作业区"}, 201)
	post("/api/zones/Z1/versions", fiber.Map{"boundary": "半径300m"}, 201)
	post("/api/units", fiber.Map{"id": "U1", "name": "加注承包一队"}, 201)
	post("/api/units", fiber.Map{"id": "U2", "name": "消防保障分队"}, 201)
	post("/api/crews", fiber.Map{"id": "C1", "name": "加注一班", "unit_id": "U1", "zone_id": "Z1", "lead_user_id": "lead1"}, 201)
	post("/api/crews", fiber.Map{"id": "C2", "name": "消防一班", "unit_id": "U2", "zone_id": "Z1", "lead_user_id": "lead2"}, 201)
	post("/api/persons", fiber.Map{"id": "P1", "name": "张加注", "post": "操作手", "unit_id": "U1", "crew_id": "C1", "zone_id": "Z1", "shift_start": "08:00", "shift_end": "20:00"}, 201)
	post("/api/persons", fiber.Map{"id": "P2", "name": "王消防", "post": "值守", "unit_id": "U2", "crew_id": "C2", "zone_id": "Z1", "shift_start": "22:00", "shift_end": "06:00"}, 201)
	post("/api/assets", fiber.Map{"id": "A1", "label": "活动门架", "kind": "gantry", "unit_id": "U1", "zone_id": "Z1"}, 201)
}

func scanBody(scanID, kind, id, dir string, at time.Time) fiber.Map {
	return fiber.Map{
		"scan_id": scanID, "subject_kind": kind, "subject_id": id,
		"gate": "GATE-1", "direction": dir, "scanned_at": at.UTC().Format(time.RFC3339),
	}
}

// 承包单位不得查看其他单位人员资料。
func TestContractorDataScope(t *testing.T) {
	app, _ := newTestApp(t)
	seedAll(t, app)

	code, resp := do(t, app, http.MethodGet, "/api/persons", nil, u1Head)
	if code != 200 {
		t.Fatalf("contractor list persons: %d %v", code, resp)
	}
	persons := resp["persons"].([]any)
	if len(persons) != 1 || persons[0].(map[string]any)["id"] != "P1" {
		t.Fatalf("U1 contractor should only see own unit persons, got %v", persons)
	}
	// 显式请求他单位参数也必须被强制限定回本单位
	code, resp = do(t, app, http.MethodGet, "/api/persons?unit_id=U2", nil, u1Head)
	if code != 200 {
		t.Fatalf("%d %v", code, resp)
	}
	persons = resp["persons"].([]any)
	if len(persons) != 1 || persons[0].(map[string]any)["unit_id"] != "U1" {
		t.Fatalf("contractor must not read other units even with explicit param, got %v", persons)
	}
	// 发射指挥无权查阅人员资料
	code, _ = do(t, app, http.MethodGet, "/api/persons", nil, lcHead)
	if code != 403 {
		t.Fatalf("launch command must not read personnel, got %d", code)
	}
}

// 发射指挥只能接收最终放行结论。
func TestLaunchCommandReadOnly(t *testing.T) {
	app, _ := newTestApp(t)
	seedAll(t, app)

	code, _ := do(t, app, http.MethodPost, "/api/zones/Z1/verify", nil, lcHead)
	if code != 403 {
		t.Fatalf("launch command must not verify zones, got %d", code)
	}
	code, _ = do(t, app, http.MethodPost, "/api/zones", fiber.Map{"id": "ZX", "name": "x"}, lcHead)
	if code != 403 {
		t.Fatalf("launch command must not create zones, got %d", code)
	}
	code, _ = do(t, app, http.MethodGet, "/api/dashboard", nil, lcHead)
	if code != 403 {
		t.Fatalf("launch command must not see command dashboard, got %d", code)
	}
	// 放行前：结论列表为空但可读
	code, resp := do(t, app, http.MethodGet, "/api/releases", nil, lcHead)
	if code != 200 || len(resp["releases"].([]any)) != 0 {
		t.Fatalf("releases should be readable and empty, got %d %v", code, resp)
	}

	// 完成放行：先核对（缺回执 → 自动冻结），再补齐回执、解冻、确认
	code, _ = do(t, app, http.MethodPost, "/api/zones/Z1/verify", nil, gcHead)
	if code != 409 { // 缺回执冻结已自动登记
		t.Fatalf("first verify should freeze on missing receipts, got %d", code)
	}
	code, resp = do(t, app, http.MethodGet, "/api/zones/Z1/clearance", nil, gcHead)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	freezes := resp["active_freezes"].([]any)
	if len(freezes) != 1 {
		t.Fatalf("expected 1 freeze, got %v", freezes)
	}
	fid := int(freezes[0].(map[string]any)["id"].(float64))
	for i, id := range []string{"P1", "P2"} {
		code, r := do(t, app, http.MethodPost, "/api/gate-receipts", scanBody(fmt.Sprintf("SC-%d", i), "person", id, "exit", apiT0.Add(-time.Hour)), gcHead)
		if code != 201 {
			t.Fatalf("scan %s: %d %v", id, code, r)
		}
	}
	do(t, app, http.MethodPost, "/api/gate-receipts", scanBody("SC-A", "asset", "A1", "exit", apiT0.Add(-time.Hour)), gcHead)
	code, _ = do(t, app, http.MethodPost, fmt.Sprintf("/api/zones/Z1/freezes/%d/resolve", fid), nil, gcHead)
	if code != 204 {
		t.Fatalf("resolve freeze: %d", code)
	}
	for _, h := range []hdr{lead1Hdr, lead2Hdr} {
		crew := "C1"
		if h["X-User-Id"] == "lead2" {
			crew = "C2"
		}
		code, r := do(t, app, http.MethodPost, "/api/crews/"+crew+"/confirmations", nil, h)
		if code != 201 {
			t.Fatalf("confirm %s: %d %v", crew, code, r)
		}
	}
	code, resp = do(t, app, http.MethodPost, "/api/zones/Z1/verify", nil, gcHead)
	if code != 200 {
		t.Fatalf("verify: %d %v", code, resp)
	}

	// 发射指挥读取最终放行结论
	code, resp = do(t, app, http.MethodGet, "/api/releases", nil, lcHead)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	releases := resp["releases"].([]any)
	if len(releases) != 1 {
		t.Fatalf("expected 1 release, got %v", releases)
	}
	rel := releases[0].(map[string]any)
	if rel["zone_id"] != "Z1" || rel["version"].(float64) != 1 || rel["conclusion"] == "" {
		t.Fatalf("bad release view: %v", rel)
	}
	code, resp = do(t, app, http.MethodGet, "/api/releases/Z1", nil, lcHead)
	if code != 200 || resp["milestone_id"] != "MS-REL-Z1-v1" {
		t.Fatalf("release detail: %d %v", code, resp)
	}
}

// 班组负责人只能确认本班组。
func TestCrewLeadConfirmScope(t *testing.T) {
	app, _ := newTestApp(t)
	seedAll(t, app)

	code, _ := do(t, app, http.MethodPost, "/api/crews/C2/confirmations", nil, lead1Hdr)
	if code != 403 {
		t.Fatalf("lead1 must not confirm C2, got %d", code)
	}
	code, resp := do(t, app, http.MethodPost, "/api/crews/C1/confirmations", nil, lead1Hdr)
	if code != 201 || resp["created"] != true {
		t.Fatalf("lead1 confirm C1: %d %v", code, resp)
	}
	// 重复确认幂等
	code, resp = do(t, app, http.MethodPost, "/api/crews/C1/confirmations", nil, lead1Hdr)
	if code != 200 || resp["created"] != false {
		t.Fatalf("re-confirm should be idempotent: %d %v", code, resp)
	}
	// 承包单位不能确认班组
	code, _ = do(t, app, http.MethodPost, "/api/crews/C2/confirmations", nil, u2Head)
	if code != 403 {
		t.Fatalf("contractor must not confirm crews, got %d", code)
	}
}

// 通知回执：承包单位只能回执本单位通知。
func TestNotificationAckScope(t *testing.T) {
	app, _ := newTestApp(t)
	seedAll(t, app)

	code, resp := do(t, app, http.MethodPost, "/api/zones/Z1/notify", nil, gcHead)
	if code != 200 {
		t.Fatalf("notify: %d %v", code, resp)
	}
	notifs := resp["notifications"].([]any)
	if len(notifs) != 3 { // P1、P2、A1 均未撤离
		t.Fatalf("expected 3 notifications, got %v", notifs)
	}
	var u1Notif, u2Notif float64
	for _, n := range notifs {
		m := n.(map[string]any)
		if m["unit_id"] == "U1" && m["subject_id"] == "P1" {
			u1Notif = m["id"].(float64)
		}
		if m["unit_id"] == "U2" {
			u2Notif = m["id"].(float64)
		}
	}
	// U2 承包单位回执 U1 的通知 → 拒绝
	code, _ = do(t, app, http.MethodPost, fmt.Sprintf("/api/notifications/%d/ack", int(u1Notif)), nil, u2Head)
	if code != 403 {
		t.Fatalf("U2 must not ack U1 notification, got %d", code)
	}
	// U1 回执本单位通知 → 成功
	code, resp = do(t, app, http.MethodPost, fmt.Sprintf("/api/notifications/%d/ack", int(u1Notif)), nil, u1Head)
	if code != 200 || resp["acked_by"] != "u1-boss" {
		t.Fatalf("U1 ack: %d %v", code, resp)
	}
	// 承包单位只能看到本单位通知
	code, resp = do(t, app, http.MethodGet, "/api/notifications", nil, u2Head)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, n := range resp["notifications"].([]any) {
		if n.(map[string]any)["unit_id"] != "U2" {
			t.Fatalf("U2 contractor sees foreign notification: %v", n)
		}
	}
	_ = u2Notif
}

// 指挥席总览与重复扫码 HTTP 行为。
func TestDashboardAndDuplicateScanHTTP(t *testing.T) {
	app, _ := newTestApp(t)
	seedAll(t, app)

	// 重复扫码：第二次返回 200 且 created=false
	code, resp := do(t, app, http.MethodPost, "/api/gate-receipts", scanBody("SC-1", "person", "P1", "exit", apiT0.Add(-time.Hour)), gcHead)
	if code != 201 || resp["created"] != true {
		t.Fatalf("first scan: %d %v", code, resp)
	}
	code, resp = do(t, app, http.MethodPost, "/api/gate-receipts", scanBody("SC-1", "person", "P1", "exit", apiT0.Add(-time.Hour)), gcHead)
	if code != 200 || resp["created"] != false {
		t.Fatalf("duplicate scan: %d %v", code, resp)
	}

	// 人员重新进入 → 冻结出现在指挥席
	code, _ = do(t, app, http.MethodPost, "/api/gate-receipts", scanBody("SC-2", "person", "P1", "entry", apiT0.Add(-30*time.Minute)), gcHead)
	if code != 201 {
		t.Fatalf("entry scan: %d", code)
	}
	code, resp = do(t, app, http.MethodGet, "/api/dashboard", nil, gcHead)
	if code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	zones := resp["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("expected 1 zone, got %v", zones)
	}
	z := zones[0].(map[string]any)
	if z["status"] != string(domain.StatusFrozen) {
		t.Fatalf("zone should be frozen after re-entry, got %v", z["status"])
	}
	freezes := z["active_freezes"].([]any)
	if len(freezes) != 1 || freezes[0].(map[string]any)["reason"] != string(domain.FreezeReEntry) {
		t.Fatalf("expected re_entry freeze on dashboard, got %v", freezes)
	}
	pending := z["pending"].([]any)
	found := false
	for _, p := range pending {
		m := p.(map[string]any)
		if m["id"] == "P1" && m["reason"] == "re_entered" && m["unit_name"] == "加注承包一队" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dashboard pending should list P1 re_entered with unit name, got %v", pending)
	}
}

// 未认证与越权登记。
func TestAuthzBasics(t *testing.T) {
	app, _ := newTestApp(t)
	code, _ := do(t, app, http.MethodGet, "/api/dashboard", nil, nil)
	if code != 401 {
		t.Fatalf("anonymous should be 401, got %d", code)
	}
	code, _ = do(t, app, http.MethodPost, "/api/zones", fiber.Map{"id": "Z9", "name": "x"}, u1Head)
	if code != 403 {
		t.Fatalf("contractor must not register zones, got %d", code)
	}
}
