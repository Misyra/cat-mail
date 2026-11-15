// gzip.go 响应 gzip 压缩中间件（W2.5/O45）：纯 stdlib（compress/gzip + sync.Pool），
// 不引第三方依赖。仅作用于响应方向（不触碰请求体，/api 上传等请求原样透传）。
package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// gzipMinSize Content-Length 已知且小于该值时不压缩：极小响应压缩后可能不降反升，
// 且白付 gzip 头尾开销；未知长度（流式，如 API JSON、CSV 导出）直接压，开销可忽略。
const gzipMinSize = 1 << 10 // 1KB

// gzipWriterPool 池化 gzip.Writer（内部含 deflate 状态），避免每请求重复分配。
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) }, // 默认压缩级别
}

// compressibleExtraTypes text/ 前缀之外的可压缩类型（base type：去参数、小写）。
var compressibleExtraTypes = map[string]struct{}{
	"application/javascript": {},
	"application/json":       {},
	"application/xml":        {},
	"image/svg+xml":          {},
}

// Gzip 响应压缩中间件。满足全部条件才压缩：
//   - 方法 GET（HEAD 无 body，天然跳过压缩，仅参与 Vary 协商标注）；
//   - Accept-Encoding 含 gzip（token 级解析，q=0 视为拒绝）；
//   - 请求无 Range 头（gzip 与 Range 不可混用，分段响应恒为未压缩直出）；
//   - 响应头无 Content-Encoding（上游已压缩则不重复压缩）；
//   - Content-Type 属可压缩集合：text/ 前缀（覆盖 text/html、text/css、
//     text/javascript、text/csv 等）及 application/javascript、application/json、
//     application/xml、image/svg+xml；
//   - Content-Length 已知且 >= 1KB（见 gzipMinSize）。
//
// 压缩候选请求（GET/HEAD 且无 Range）无论最终是否压缩，统一加
// Vary: Accept-Encoding，避免共享缓存把压缩/未压缩变体串味。
//
// 判定时机：包装器的 WriteHeader 与首次 Write 都会触发 decide（各执行一次）。
// gin 的 WriteHeader 是延迟提交（首次写才真正推送响应头），且 c.JSON 这类渲染
// 流程先 Status 后写 Content-Type——故 WriteHeader 时若 Content-Type 未定，
// 判定推迟到首次写（届时响应头已齐）再执行。
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			c.Next()
			return
		}
		if req.Header.Get("Range") != "" {
			// Range 与 gzip 不可混用：压缩响应无法按字节对位分段，直接不参与协商。
			c.Next()
			return
		}
		// 候选请求统一标注 Vary（含不带 gzip 的与 HEAD 的），供缓存正确区分变体。
		if v := c.Writer.Header().Get("Vary"); !containsToken(v, "Accept-Encoding") {
			c.Writer.Header().Add("Vary", "Accept-Encoding")
		}
		if req.Method != http.MethodGet || !acceptsGzip(req.Header.Get("Accept-Encoding")) {
			c.Next()
			return
		}
		w := &gzipResponseWriter{ResponseWriter: c.Writer}
		c.Writer = w
		// panic 安全：close 挂在本中间件帧上，handler panic 沿栈展开时也会先落
		// gzip 尾部并归还池化 writer，随后由外层 api.Recovery 统一兜底 500。
		defer w.close()
		c.Next()
	}
}

// gzipResponseWriter 包装 gin.ResponseWriter：响应头定型（WriteHeader / 首次写）
// 时机判定是否接管写入，接管后经 gzip.Writer 压缩，否则原样透传。
type gzipResponseWriter struct {
	gin.ResponseWriter
	gw      *gzip.Writer // 已接管时非 nil
	decided bool         // 已完成压缩判定（响应头定型后不再改变）
}

// decide 压缩判定，仅执行一次。code 限定 200：204/304 无 body、错误体极小无收益、
// 206 已被 Range 前置排除。激活时删除 Content-Length（压缩后长度已变，不可信）、
// 标注 Content-Encoding: gzip。
func (g *gzipResponseWriter) decide(code int) {
	if g.decided {
		return
	}
	if code != http.StatusOK {
		g.decided = true
		return
	}
	h := g.ResponseWriter.Header()
	if h.Get("Content-Encoding") != "" {
		g.decided = true
		return
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		// gin 渲染流程先 Status 后写 Content-Type：等首次写时再判
		return
	}
	g.decided = true
	if !compressibleContentType(ct) {
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n < gzipMinSize {
			return
		}
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	g.gw = gzipWriterPool.Get().(*gzip.Writer)
	g.gw.Reset(g.ResponseWriter)
}

// compressibleContentType 判断响应 Content-Type 是否可压缩（取分号前的 base
// type 比对，忽略 charset 等参数）。
func compressibleContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	_, ok := compressibleExtraTypes[ct]
	return ok
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.decide(code)
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	// 未显式 WriteHeader 的流式响应：gin 默认状态 200，首写时惰性判定。
	g.decide(g.ResponseWriter.Status())
	if g.gw != nil {
		return g.gw.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) WriteString(s string) (int, error) {
	g.decide(g.ResponseWriter.Status())
	if g.gw != nil {
		return g.gw.Write([]byte(s))
	}
	return g.ResponseWriter.WriteString(s)
}

// Flush 透传流式刷新（SSE/CSV 等）：先压出 gzip 内部缓冲，再刷底层写入器。
func (g *gzipResponseWriter) Flush() {
	if g.gw != nil {
		_ = g.gw.Flush()
	}
	g.ResponseWriter.Flush()
}

// close 落 gzip 尾部（CRC 与长度校验码）并归还池化 writer；由 Gzip 的 defer 在
// handler 返回（含 panic 展开）后调用。Close 失败多为客户端中途断开，响应已残缺
// 无可补救，静默即可。
func (g *gzipResponseWriter) close() {
	if g.gw == nil {
		return
	}
	_ = g.gw.Close()
	g.gw.Reset(io.Discard)
	gzipWriterPool.Put(g.gw)
	g.gw = nil
}

// acceptsGzip 解析 Accept-Encoding：存在任一 gzip/x-gzip 条目且其 q≠0 时为 true
// （RFC 9110 §12.5.3：q 缺省按 1，q=0 表示明确拒绝）。
func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		enc, params, _ := strings.Cut(strings.ToLower(strings.TrimSpace(part)), ";")
		if enc != "gzip" && enc != "x-gzip" {
			continue
		}
		if q := qValue(params); q == 0 {
			continue
		}
		return true
	}
	return false
}

// qValue 从 ";q=0.5" 之类的参数串解析 q 值，缺失或畸形按 1（宽容处理）。
func qValue(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		name, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if name == "q" {
			if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
				return v
			}
		}
	}
	return 1
}

// containsToken 判断逗号分隔的头值列表（如 Vary）是否含指定 token（大小写不敏感）。
func containsToken(list, token string) bool {
	token = strings.ToLower(token)
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
