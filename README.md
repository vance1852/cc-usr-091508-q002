# 发射场勤务区撤离放行

本项目面向发射场地面保障和指挥团队，用于核对倒计时管制前各危险区的人员、车辆与设备撤离情况。

系统需要保留区域版本、班组确认和门禁回执，使冻结、复核与最终放行都有清晰的安全依据。

## 架构

```
cmd/server/        服务入口（Fiber HTTP，SQLite 持久化）
internal/domain/   核心实体：危险区版本、名册、回执、确认、冻结、里程碑、通知
internal/store/    SQLite 数据访问（幂等约束 + 事务，进程重启后状态完整恢复）
internal/service/  业务规则：清场评估、冻结/解冻、核对放行、数据范围控制
internal/api/      Fiber 路由与 RBAC
```

技术栈：Go 1.27 · Fiber v2 · SQLite（mattn/go-sqlite3，WAL + 外键 + 单连接串行化）。

## 运行

```bash
go build -o evac-server ./cmd/server
./evac-server -db evac.db -addr :8080 -seed   # -seed 写入演示数据
```

身份通过请求头传入：`X-User-Id`、`X-Role`、`X-Unit-Id`（承包单位）、`X-Crew-Id`（班组负责人）。

## 角色与权限

| 角色 | 权限 |
|---|---|
| `crew_lead` 班组负责人 | 仅确认本班组撤离状态 |
| `ground_command` 地面保障指挥 | 登记档案、核对区域、冻结解除、通知、里程碑、指挥席总览 |
| `launch_command` 发射指挥 | 只读接收最终放行结论与里程碑偏差 |
| `contractor` 承包单位 | 仅本单位人员资料与通知回执，不得查看其他单位 |

## 核心规则

- **区域版本**：边界扩大发布新版本并立即冻结；班组确认按版本记录，新版本须重新确认。
- **门禁回执**：`scan_id` 幂等，重复扫码返回原回执；离场后再次进入立即冻结。
- **冻结放行**：缺少回执（核对时自动登记）、人员重新进入、区域边界扩大；诱因消除后由地面保障指挥解除。
- **放行**：全部对象离场 + 班组全部确认 + 无活动冻结 + 依赖区域已放行 → 生成最终结论并自动封存关联里程碑；重复核对幂等。
- **里程碑**：封存后结论不可改，只能追加偏差说明。
- **指挥席**：`/api/dashboard` 实时呈现各区域最后确认时间、未撤离对象（含责任单位）、冻结原因与通知回执。

## 主要接口

```
POST /api/zones · /api/zones/:id/versions · /api/zones/:id/dependencies
POST /api/units · /api/crews · /api/persons · /api/assets
POST /api/gate-receipts                门禁回执（幂等）
POST /api/crews/:id/confirmations      班组确认（班组负责人）
GET  /api/zones/:id/clearance          区域清场明细（未撤离对象+责任单位+依赖状态）
POST /api/zones/:id/verify             核对并放行
POST /api/zones/:id/freezes/:fid/resolve  解除冻结
POST /api/zones/:id/notify             向责任单位发撤离通知
GET  /api/dashboard                    指挥席总览
GET  /api/releases · /api/releases/:zoneId   最终放行结论（发射指挥）
GET  /api/persons                      人员资料（按角色限定数据范围）
GET  /api/notifications · POST /api/notifications/:id/ack
POST /api/zones/:id/milestones · /api/milestones/:id/seal · /api/milestones/:id/deviations
```

## 自动化场景验证

```bash
go test ./...
```

- `TestDuplicateScanIdempotent` —— 重复扫码幂等，不产生重复回执/冻结
- `TestConcurrentConfirmationsAndScans` —— 并发班组确认、并发同码上报、并发核对均一致收敛
- `TestCrossMidnightShift` —— 跨午夜班次（22:00–06:00）凌晨扫码正确计入清场与当班判定
- `TestRecoveryAfterRestart` —— 进程重启后未决清场事项、冻结与确认完整恢复并继续推进
- `TestFullReleaseFlow` —— 缺回执冻结、区域依赖、里程碑封存、边界扩大重新确认全链路
- `internal/api` —— RBAC：承包单位数据隔离、发射指挥只读、班组负责人越组确认拒绝
