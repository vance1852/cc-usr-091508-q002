package core

import (
	"database/sql"
	"time"
)

// Seed 在空库中注入演示数据：三个危险区（含依赖）、加注/消防/测量班组、
// 人员车辆名册、跨午夜班次与倒计时里程碑。幂等：已有数据时不动作。
func Seed(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM actors`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	exec := func(q string, args ...any) error {
		_, err := tx.Exec(q, args...)
		return err
	}

	// 责任单位
	for _, u := range [][2]string{
		{"unit-alpha", "甲安航天保障公司"},
		{"unit-beta", "乙联地面勤务公司"},
	} {
		if err := exec(`INSERT INTO units (id, name) VALUES (?,?)`, u[0], u[1]); err != nil {
			return err
		}
	}
	// 班组
	for _, c := range [][4]string{
		{"crew-fuel", "加注组", "unit-alpha", "fueling"},
		{"crew-fire", "消防组", "unit-beta", "fire"},
		{"crew-measure", "测量组", "unit-beta", "measurement"},
	} {
		if err := exec(`INSERT INTO crews (id, name, unit_id, specialty) VALUES (?,?,?,?)`, c[0], c[1], c[2], c[3]); err != nil {
			return err
		}
	}
	// 操作者（演示账号，生产环境应接入统一认证）
	for _, a := range [][5]string{
		{"gc-01", "地面保障指挥", string(RoleGroundCommander), "", ""},
		{"lc-01", "发射指挥", string(RoleLaunchCommander), "", ""},
		{"lead-fuel", "加注组长", string(RoleCrewLead), "unit-alpha", "crew-fuel"},
		{"lead-fire", "消防组长", string(RoleCrewLead), "unit-beta", "crew-fire"},
		{"lead-measure", "测量组长", string(RoleCrewLead), "unit-beta", "crew-measure"},
		{"ctr-alpha", "甲安公司调度", string(RoleContractor), "unit-alpha", ""},
		{"ctr-beta", "乙联公司调度", string(RoleContractor), "unit-beta", ""},
		{"gate-sys", "门禁系统", string(RoleGateOperator), "", ""},
	} {
		if err := exec(`INSERT INTO actors (id, name, role, unit_id, crew_id) VALUES (?,?,?,?,?)`,
			a[0], a[1], a[2], a[3], a[4]); err != nil {
			return err
		}
	}
	// 危险区与依赖：发射工位区依赖加注区先清场
	for _, z := range [][2]string{
		{"Z-FUEL", "加注作业区"},
		{"Z-PAD", "发射工位区"},
		{"Z-ASSY", "总装测试区"},
	} {
		if err := exec(`INSERT INTO zones (id, name) VALUES (?,?)`, z[0], z[1]); err != nil {
			return err
		}
	}
	if err := exec(`INSERT INTO zone_deps (zone_id, depends_on) VALUES ('Z-PAD','Z-FUEL')`); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, z := range []string{"Z-FUEL", "Z-PAD", "Z-ASSY"} {
		if err := exec(`INSERT INTO zone_versions (zone_id, version, boundary, expanded, created_at) VALUES (?,1,?,0,?)`,
			z, z+" 初始边界", now); err != nil {
			return err
		}
	}
	// 人员名册
	for _, p := range [][5]string{
		{"P-101", "王加注", "unit-alpha", "crew-fuel", "加注操作手"},
		{"P-102", "李管路", "unit-alpha", "crew-fuel", "管路巡检"},
		{"P-103", "赵消防", "unit-beta", "crew-fire", "消防值守"},
		{"P-104", "孙测量", "unit-beta", "crew-measure", "大地测量"},
		{"P-105", "周水准", "unit-beta", "crew-measure", "水准测量"},
	} {
		if err := exec(`INSERT INTO personnel (id, name, unit_id, crew_id, post) VALUES (?,?,?,?,?)`,
			p[0], p[1], p[2], p[3], p[4]); err != nil {
			return err
		}
	}
	// 车辆设备（含活动门架）
	for _, v := range [][5]string{
		{"V-201", "京A1001", "加注车", "unit-alpha", "crew-fuel"},
		{"V-202", "京A1002", "消防车", "unit-beta", "crew-fire"},
		{"G-301", "门架-01", "活动门架", "unit-beta", "crew-measure"},
	} {
		if err := exec(`INSERT INTO vehicles (id, plate, kind, unit_id, crew_id) VALUES (?,?,?,?,?)`,
			v[0], v[1], v[2], v[3], v[4]); err != nil {
			return err
		}
	}
	// 跨午夜班次：前一日 22:00 至当日 06:00
	day := time.Now().Truncate(24 * time.Hour)
	nightStart := day.Add(-2 * time.Hour).Unix() // 昨日 22:00
	nightEnd := day.Add(6 * time.Hour).Unix()    // 今日 06:00
	if err := exec(`INSERT INTO shifts (id, name, starts_at, ends_at) VALUES ('shift-night','夜班(22:00-06:00)',?,?)`,
		nightStart, nightEnd); err != nil {
		return err
	}
	// 倒计时里程碑
	for _, m := range [][3]string{
		{"M1", "运载火箭转运就位", "1"},
		{"M2", "勤务区撤离完成", "2"},
		{"M3", "倒计时管制切换", "3"},
	} {
		if err := exec(`INSERT INTO milestones (id, name, seq) VALUES (?,?,?)`, m[0], m[1], m[2]); err != nil {
			return err
		}
	}
	// 区域作业派遣（清场核对基准名册）
	for _, a := range [][6]string{
		{"Z-FUEL", "person", "P-101", "crew-fuel", "unit-alpha", "shift-night"},
		{"Z-FUEL", "person", "P-102", "crew-fuel", "unit-alpha", "shift-night"},
		{"Z-FUEL", "vehicle", "V-201", "crew-fuel", "unit-alpha", "shift-night"},
		{"Z-PAD", "person", "P-103", "crew-fire", "unit-beta", "shift-night"},
		{"Z-PAD", "vehicle", "V-202", "crew-fire", "unit-beta", "shift-night"},
		{"Z-ASSY", "person", "P-104", "crew-measure", "unit-beta", "shift-night"},
		{"Z-ASSY", "person", "P-105", "crew-measure", "unit-beta", "shift-night"},
		{"Z-ASSY", "vehicle", "G-301", "crew-measure", "unit-beta", "shift-night"},
	} {
		if err := exec(`INSERT INTO assignments (zone_id, object_type, object_id, crew_id, unit_id, shift_id) VALUES (?,?,?,?,?,?)`,
			a[0], a[1], a[2], a[3], a[4], a[5]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
