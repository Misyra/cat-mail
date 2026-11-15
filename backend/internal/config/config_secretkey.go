// config_secretkey.go 渠道凭据加密密钥（CATMAIL_SECRET_KEY，安全加固三件套）：
// base64 编码 32 字节（AES-256-GCM）。未设置 = 渠道凭据明文存储（现状兼容），
// doctor 会给 WARN 提示建议配置；设置但非法（base64 解码失败 / 长度 ≠ 32）在
// Load 时 log.Fatalf 快速失败，绝不带病启动（加密密钥配错比不生效更危险——
// 起一半的加密库既解不开旧密文、也写不出可信新密文）。
package config

import (
	"log"

	"catmail/internal/secretcrypt"
)

// mustSecretKey 解析 CATMAIL_SECRET_KEY：未设置返回 nil（明文兼容模式，现状）；
// 设置但非法直接终止进程（ParseKey 校验 base64 与 32 字节长度）。
func mustSecretKey(raw string) []byte {
	if raw == "" {
		return nil
	}
	key, err := secretcrypt.ParseKey(raw)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	return key
}
