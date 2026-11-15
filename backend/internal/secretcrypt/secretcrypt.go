// Package secretcrypt 实现渠道凭据的静态加密（AES-256-GCM，安全加固三件套）：
// 密钥来自环境变量 CATMAIL_SECRET_KEY（base64 编码 32 字节），未配置 = 明文
// 读写兼容（现状）；配置后 Channel 表敏感字段在 store 读写路径透明加解密
// （经 gorm serializer 收敛在模型访问层一处，业务代码无感知）。
//
// 密文格式：enc:v1:<b64nonce>:<b64ct>（base64 均为 std 编码；nonce 12B，GCM 认证
// 标签随密文）。加密收敛为函数级单点：Encrypt/Decrypt 只在此包出现，杜绝散落的
// 手写加解密；aad 绑定格式版本串，防密文跨域挪用。
//
// 线程安全：全局密钥经 atomic.Pointer 保存，进程生命周期内只在启动期设置一次
// （config.Load → SetGlobalKey，先于任何业务查询）；doctor 子命令同样先设后查。
package secretcrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"gorm.io/gorm/schema"
)

// Prefix 密文前缀（enc:v1:<b64nonce>:<b64ct>）：HasPrefix 判定是否已是密文。
const Prefix = "enc:v1:"

// KeySize 密钥字节长度（AES-256 要求 32 字节；CATMAIL_SECRET_KEY 解码后必须恰好）。
const KeySize = 32

// aad GCM 关联数据：绑定本格式版本，密文跨域挪用（如改列名/换用途）时解密失败。
var aad = []byte("catmail:channel-credential:v1")

// ErrNoKey 未配置全局密钥时尝试解密密文（调用方应拒绝启动而非静默返回乱码）。
var ErrNoKey = errors.New("secretcrypt: CATMAIL_SECRET_KEY 未配置，无法解密已加密凭据")

// ErrBadPayload 密文格式损坏/认证失败（密钥不匹配或数据被篡改）。
var ErrBadPayload = errors.New("secretcrypt: 密文无效或密钥不匹配")

// globalKey 进程级密钥（nil = 未配置，明文兼容模式）。
var globalKey atomic.Pointer[[]byte]

// SetGlobalKey 设置进程级密钥（调用方保证已过 ParseKey 校验；key 内部拷贝，防外部
// 改写；nil/空 = 清除密钥，回到明文兼容模式）。
func SetGlobalKey(key []byte) {
	if len(key) == 0 {
		globalKey.Store(nil)
		return
	}
	k := make([]byte, len(key))
	copy(k, key)
	globalKey.Store(&k)
}

// GlobalKeySet 报告是否已配置全局密钥。
func GlobalKeySet() bool { return globalKey.Load() != nil }

// ParseKey 校验 CATMAIL_SECRET_KEY 原始值：base64（std 编码）解码后必须恰好
// 32 字节（AES-256）。任何不满足都返回错误——config.Load 据此快速失败，绝不带病启动。
func ParseKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("CATMAIL_SECRET_KEY 为空")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("CATMAIL_SECRET_KEY 不是合法 base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("CATMAIL_SECRET_KEY 解码后须为 %d 字节（当前 %d 字节；请提供 base64 编码的 32 字节随机数，如 openssl rand -base64 32）", KeySize, len(key))
	}
	return key, nil
}

// Encrypt 全局密钥加密（模型写路径入口）：空值原样返回（空串=未配置，无需加密）；
// 已带密文前缀原样返回（幂等，防 read-modify-write 场景二次加密）；未配置密钥
// 原样返回（明文兼容模式，现状行为）。加密失败 panic 不外泄——值非法只可能是
// 程序缺陷，按零值兼容串处理会静默破坏数据，fail-fast 于日志更诚实。
func Encrypt(plaintext string) string {
	key := globalKey.Load()
	if key == nil || plaintext == "" || strings.HasPrefix(plaintext, Prefix) {
		return plaintext
	}
	ct, err := EncryptWith(*key, plaintext)
	if err != nil {
		panic(fmt.Sprintf("secretcrypt: encrypt: %v", err))
	}
	return ct
}

// Decrypt 全局密钥解密（模型读路径入口）：未带前缀原样返回（明文兼容：存量迁移
// 前的旧行、或未配置密钥的部署）；带前缀但未配置密钥 → ErrNoKey（调用方终止，
// 避免「永远解不开」的静默坏数据）；解密/认证失败 → ErrBadPayload。
func Decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, Prefix) {
		return value, nil
	}
	key := globalKey.Load()
	if key == nil {
		return "", ErrNoKey
	}
	return DecryptWith(*key, value)
}

// EncryptWith 显式密钥加密（迁移与测试用，不经全局态）。
func EncryptWith(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretcrypt: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), aad)
	return Prefix + base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// DecryptWith 显式密钥解密（迁移与测试用）：value 必须带密文前缀。
func DecryptWith(key []byte, value string) (string, error) {
	rest, ok := strings.CutPrefix(value, Prefix)
	if !ok {
		return "", errors.New("secretcrypt: 非密文格式（缺少 enc:v1: 前缀）")
	}
	nonceB64, ctB64, ok := strings.Cut(rest, ":")
	if !ok {
		return "", ErrBadPayload
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return "", ErrBadPayload
	}
	ct, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		return "", ErrBadPayload
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", ErrBadPayload
	}
	return string(pt), nil
}

// newGCM 构建 AES-256-GCM 实例（密钥长度在 ParseKey 保证；防御性再校验）。
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretcrypt: 密钥长度须为 %d 字节（当前 %d）", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ---------- gorm serializer（模型访问层收敛点） ----------

// SerializerName gorm 字段标签取值：gorm:"serializer:encsecret"。
const SerializerName = "encsecret"

func init() { schema.RegisterSerializer(SerializerName, GormSerializer{}) }

// GormSerializer gorm 透明加解密序列化器：写路径 ValueOf 经 Encrypt、读路径 Scan
// 经 Decrypt——业务代码与各查询路径（Create/Updates(struct)/First/Find）无感知。
// 注意：gorm 的 Updates(map) 不经 serializer（写原始值），全库对该字段的 map 写
// 只有迁移一处（写已加密值，见 store/encryption.go），新增 map 写必须走本包加密。
type GormSerializer struct{}

// Scan 读路径：DB 原始值（string/[]byte）→ Decrypt → 写入结构体字段。
// nil/空值保持零值；解密失败返回错误（查询整体失败——密钥不匹配属部署事故，
// 宁可显式报错也不让业务拿到垃圾串）。
func (GormSerializer) Scan(ctx context.Context, field *schema.Field, dst reflect.Value, dbValue any) error {
	if dbValue == nil {
		return nil
	}
	var raw string
	switch v := dbValue.(type) {
	case []byte:
		raw = string(v)
	case string:
		raw = v
	default:
		return fmt.Errorf("secretcrypt: 不支持的列值类型 %T", dbValue)
	}
	pt, err := Decrypt(raw)
	if err != nil {
		return err
	}
	field.ReflectValueOf(ctx, dst).SetString(pt)
	return nil
}

// Value 写路径：结构体字段值 → Encrypt → DB 值（始终 string，无需判空）。
func (GormSerializer) Value(_ context.Context, _ *schema.Field, _ reflect.Value, fieldValue any) (any, error) {
	s, ok := fieldValue.(string)
	if !ok {
		if fieldValue == nil {
			return "", nil
		}
		return nil, fmt.Errorf("secretcrypt: 字段类型须为 string（当前 %T）", fieldValue)
	}
	return Encrypt(s), nil
}
