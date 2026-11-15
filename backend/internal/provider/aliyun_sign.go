// aliyun_sign.go 阿里云 POP（RPC 风格）请求签名。
// 算法与官方 SDK aliyun/alibaba-cloud-sdk-go sdk/auth/rpc_signature_composer.go 的
// buildRpcStringToSign + ShaHmac1 逐字对齐（Apache-2.0）：
//  1. 合并 query+form 参数，url.Values.Encode() 得到按 key 字典序的 k1=v1&k2=v2；
//  2. 官方 PercentEncode 特殊规则修正：'+'→'%20'、'*'→'%2A'、'%7E'→'~'（空格转 %20 是关键差异）；
//  3. 对整个规范化串再做一次 URL 全量编码（文档称"二次编码"，% 与 & 均被转义）；
//  4. StringToSign = HTTPMethod + "&%2F&" + <上述串>；
//  5. Signature = Base64(HMAC-SHA1(key=AccessKeySecret+"&", StringToSign))。
package provider

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// popTimestampFormat 阿里云公共参数 Timestamp 的格式：UTC ISO8601（官方 SDK GetTimeInFormatISO8601 同款）。
const popTimestampFormat = "2006-01-02T15:04:05Z"

// popStringToSign 构造 RPC 签名原文（见包注释）。
func popStringToSign(method string, params url.Values) string {
	canon := params.Encode()
	canon = strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(canon)
	return method + "&%2F&" + url.QueryEscape(canon)
}

// popSign 计算 HMAC-SHA1 签名：key 为 AccessKeySecret 追加 "&"，结果 Base64。
func popSign(stringToSign, accessKeySecret string) string {
	mac := hmac.New(sha1.New, []byte(accessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// popTimestamp 当前 UTC 时间的公共参数 Timestamp 值。
func popTimestamp() string {
	return time.Now().UTC().Format(popTimestampFormat)
}

// popNonce 签名随机串（官方要求每次请求唯一，uuid 足够）。
func popNonce() string {
	return uuid.NewString()
}
