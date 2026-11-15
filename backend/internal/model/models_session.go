// models_session.go 服务端会话注册表（安全加固三件套，C）：登录会话落库、
// 可查询/可吊销。会话 token 升级 v3 格式 `v3|exp|username|user_epoch|session_id|sig`
// （对齐 O10 v2 结构的版本前缀机制），注册表行是签发态的事实来源——requireSession
// 校验签名与纪元之外，再查注册表（未吊销、未过期），使「踢会话」成为服务端可执行
// 动作。SQLite | PostgreSQL 同构（无方言专属类型，AutoMigrate 直建）。
package model

import "time"

// SessionTokenV3 会话 token 版本前缀（v3，安全加固三件套）：版本前缀机制使 v2 与
// 更早格式的存量 token 天然失效——升级后重新登录一次即可（对齐 O10 v2 上线先例，
// 零数据迁移）。
const SessionTokenV3 = "v3"

// AuditSessionRevoke 会话吊销审计动作（对齐现有「资源_动作」命名与 detail 风格）：
// 本人踢指定会话 / 下线其余全部会话共用一个动作，detail 区分 kick/revoke_others。
const AuditSessionRevoke = "sessions_revoke"

// AuditPasswordResetLinkage 密码重置联动审计动作：管理员重置他人密码成功后的
// 联动三件套（纪元递增 + 会话吊销 + Key 停用）单独落一条，detail 记计数。
const AuditPasswordResetLinkage = "password_reset_linkage"

// ApiKeyDisabledByPasswordReset ApiKey.DisabledReason 的联动停用标记（管理页据此
// 展示「密码重置后停用，可手动启用」徽标；手动启用即清空）。
const ApiKeyDisabledByPasswordReset = "password_reset"

// Session 服务端会话注册表：一行对应一次登录签发的会话 token。
//
// 索引取舍：username 建索引——「本人会话列表」「重置联动批量吊销」「revoke-others」
// 均按 username 等值/批量圈定；expires_at 建索引——scheduler 清理过期行按范围扫描；
// revoked_at 非索引（吊销行占比极小，且批量吊销按 username 索引圈定后逐行过滤即可）。
type Session struct {
	ID string `gorm:"primaryKey;size:64"` // 32 字节随机 hex（256bit，碰撞不可行）
	// Username 会话属主（token 载荷中的 username 冗余，注册表校验须与之一致）。
	Username string `gorm:"index;size:64"`
	IP       string `gorm:"size:64"` // 登录来源 c.ClientIP()（展示用）
	// UserAgent 登录时的 User-Agent 原样截断存储（展示「哪台设备」用，可伪造仅作参考）。
	UserAgent string `gorm:"size:512"`
	CreatedAt time.Time
	// LastSeenAt 最近一次请求经 requireSession 惰性刷新（节流 60s，避免每请求一写）。
	LastSeenAt time.Time
	// ExpiresAt 与 token exp 语义一致（登录 +7d；改密/TOTP 重签当前会话时同步顺延）。
	ExpiresAt time.Time `gorm:"index"`
	// RevokedAt 非 nil = 已吊销（踢出/登出/重置联动），requireSession 直接拒绝。
	RevokedAt *time.Time `gorm:"index"`
}
