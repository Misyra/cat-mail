// encryption.go 渠道凭据加密的启动期检查与存量迁移（安全加固三件套，A）。
// 语义（任务书口径）：
//   - CATMAIL_SECRET_KEY 未配置 + 库里已有密文 → 返回 ErrCiphertextNeedsKey，
//     cmd 层启动退出并明确报错（避免「永远解不开」的坏库继续被写坏）；
//   - 已配置 → 一次性存量迁移（明文→密文），单事务、幂等（已带 enc: 前缀的值
//     仅做可解密校验：密钥与存量密文不匹配同样报错终止）。
//
// 实现注意：迁移刻意走「原始列值」读写（扫描进无 gorm 标签的影子结构、经 Updates(map)
// 回写），不经 Channel 模型的 encsecret serializer——map 更新不触发 serializer（gorm
// 语义），恰好让我们自己控制加密时机与密钥校验；serializer 只负责日常读写路径。
package store

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/secretcrypt"
)

// ErrCiphertextNeedsKey 未配置 CATMAIL_SECRET_KEY 但库里存在密文凭据：
// cmd 层须启动退出（明文兼容模式解不开这些字段，继续运行只会产出更多坏数据）。
var ErrCiphertextNeedsKey = errors.New(
	"数据库中存在加密凭据（enc:v1: 前缀）但未配置 CATMAIL_SECRET_KEY——请配置与加密时相同的密钥后启动")

// ErrKeyMismatch 已配置密钥与存量密文不匹配（解密认证失败）：
// cmd 层须启动退出，提示换回原密钥或手动处置。
var ErrKeyMismatch = errors.New(
	"CATMAIL_SECRET_KEY 与数据库中已有密文凭据不匹配（解密校验失败）——请确认配置的是当初加密时使用的密钥")

// channelSecretColumns 渠道表敏感字段列名（与 model.Channel 的 encsecret 字段一一对应）。
var channelSecretColumns = []string{"api_key", "api_secret", "proxy_url", "smtp_pass", "webhook_secret"}

// channelSecretRow 迁移用的影子行（无 gorm 标签 → 读到原始列值，不经 serializer 解密）。
type channelSecretRow struct {
	ID            uint
	APIKey        string
	APISecret     string
	ProxyURL      string
	SMTPPass      string
	WebhookSecret string
}

// encCols 迁移字段的 (列名, 取值) 对。
func (r *channelSecretRow) values() map[string]*string {
	return map[string]*string{
		"api_key":        &r.APIKey,
		"api_secret":     &r.APISecret,
		"proxy_url":      &r.ProxyURL,
		"smtp_pass":      &r.SMTPPass,
		"webhook_secret": &r.WebhookSecret,
	}
}

// loadChannelSecretRows 读取全部渠道的敏感列原始值。
func loadChannelSecretRows(db *gorm.DB) ([]channelSecretRow, error) {
	var rows []channelSecretRow
	err := db.Model(&model.Channel{}).
		Select(append([]string{"id"}, channelSecretColumns...)).
		Order("id").Find(&rows).Error
	return rows, err
}

// EncryptionStatus 存量加密状态盘点（doctor 检查项用）：按原始列值统计明文/密文行数
// （一行任一敏感字段为密文即计入密文行；任一敏感字段为明文即计入明文行）。
type EncryptionStatus struct {
	PlaintextRows  int // 含明文凭据的渠道行数（配置密钥时即待迁移量）
	CiphertextRows int // 含密文凭据的渠道行数（未配置密钥时 >0 即拒绝启动）
	TotalRows      int
}

// CheckChannelEncryption 盘点渠道凭据加密状态（只读，双方言通用）。
// 未配置密钥时明文行属现状兼容（0=全空或全密文），密文行 >0 即 ErrCiphertextNeedsKey
// 语义的前兆——本函数只盘点不判定，判定权在调用方（cmd/doctor）。
func CheckChannelEncryption(db *gorm.DB) (EncryptionStatus, error) {
	rows, err := loadChannelSecretRows(db)
	if err != nil {
		return EncryptionStatus{}, fmt.Errorf("盘点渠道凭据加密状态: %w", err)
	}
	st := EncryptionStatus{TotalRows: len(rows)}
	for _, r := range rows {
		plain, ciphered := false, false
		for _, p := range r.values() {
			if *p == "" {
				continue
			}
			if strings.HasPrefix(*p, secretcrypt.Prefix) {
				ciphered = true
			} else {
				plain = true
			}
		}
		if plain {
			st.PlaintextRows++
		}
		if ciphered {
			st.CiphertextRows++
		}
	}
	return st, nil
}

// EnsureChannelEncryption 启动期入口（cmd 层在 secretcrypt.SetGlobalKey 之后调用）：
//   - key==nil：库里有密文 → ErrCiphertextNeedsKey（明文兼容模式拒绝启动）；
//   - key!=nil：单事务迁移——明文值加密回写；已是密文的值试解密校验（失败 →
//     ErrKeyMismatch 回滚）。幂等：重复执行无变更（全部跳过）。
//
// 返回迁移前盘点 status 与实际加密的行数 migrated（供启动日志与 doctor 呈现）。
func EnsureChannelEncryption(db *gorm.DB, key []byte) (status EncryptionStatus, migrated int, err error) {
	status, err = CheckChannelEncryption(db)
	if err != nil {
		return status, 0, err
	}
	if key == nil {
		if status.CiphertextRows > 0 {
			return status, 0, ErrCiphertextNeedsKey
		}
		return status, 0, nil // 明文兼容模式（现状），无事可做
	}
	if status.PlaintextRows == 0 && status.CiphertextRows == 0 {
		return status, 0, nil // 空库/全空字段：无迁移必要
	}
	rows, err := loadChannelSecretRows(db)
	if err != nil {
		return status, 0, fmt.Errorf("读取渠道凭据: %w", err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			updates := map[string]any{}
			for col, p := range r.values() {
				v := *p
				if v == "" {
					continue
				}
				if strings.HasPrefix(v, secretcrypt.Prefix) {
					// 已是密文：试解密校验密钥匹配（不匹配 → 整体回滚并报错）
					if _, derr := secretcrypt.DecryptWith(key, v); derr != nil {
						return fmt.Errorf("%w（渠道 #%d 的 %s 列）", ErrKeyMismatch, r.ID, col)
					}
					continue
				}
				ct, eerr := secretcrypt.EncryptWith(key, v)
				if eerr != nil {
					return fmt.Errorf("加密渠道 #%d 的 %s: %w", r.ID, col, eerr)
				}
				updates[col] = ct
			}
			if len(updates) == 0 {
				continue
			}
			// map 更新不经 serializer（gorm 语义），密文原样落库——见文件头注释。
			if err := tx.Model(&model.Channel{}).Where("id = ?", r.ID).Updates(updates).Error; err != nil {
				return err
			}
			migrated++
		}
		return nil
	})
	if err != nil {
		return status, migrated, err
	}
	return status, migrated, nil
}
