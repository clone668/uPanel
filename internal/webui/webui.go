// Package webui 提供嵌入在 Web 二进制里的 Vue 前端资源，以及 SPA 的静态服务行为。
//
// 设计理由（PLAN.md 第 3 节：两个 Go 二进制 + 前端嵌入 Web 二进制，生产机不装 Node.js）：
//   - 前端产物通过 go:embed 打进 upanel-web，部署只拷贝二进制；
//   - 没有前端产物时**不伪装成正常页面**，一律 503 并说明原因（P01 验收：
//     “无依赖时不伪装成正常”）。仓库里 internal/webui/dist 只有占位文件，
//     真正的产物由 scripts/build.sh 在构建前同步进来；
//   - 运行模式由服务端注入 index.html（window.__UPANEL_MODE__），前端不再靠猜测
//     （见 docs/DECISIONS.md Q21）；
//   - 静态响应带 CSP 等安全头，inline 模式脚本使用一次性 nonce，不用 'unsafe-inline'。
package webui

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed all:dist
var embedded embed.FS

// distRoot 是嵌入目录名，对应 web/dist 的同步目标 internal/webui/dist。
//
// 仓库里该目录只有一个 .keep 占位文件（让 go:embed 在尚未构建前端时也能编译通过）；
// 真正的产物由 scripts/build.sh 在 go build 之前同步进来。
const distRoot = "dist"

// indexFile 是入口页文件名。
const indexFile = "index.html"

// reasonNotBuilt 是前端产物缺失时对外的说明。
const reasonNotBuilt = "此二进制的构建产物中没有前端资源；请在构建机上执行 scripts/build.sh（先构建 web 再同步到 internal/webui/dist）"

// Assets 是一组前端静态资源。
type Assets struct {
	fsys      fs.FS
	available bool
	reason    string
}

// Embedded 返回编译进二进制的资源。总是返回非 nil；是否真的有产物看 Available。
func Embedded() *Assets {
	sub, err := fs.Sub(embedded, distRoot)
	if err != nil {
		return &Assets{fsys: nil, available: false, reason: "嵌入目录损坏: " + err.Error()}
	}
	return FromFS(sub)
}

// FromFS 用给定的文件系统构造资源（测试与将来的外部资源目录用）。
// fsys 必须以 index.html 为根，例如 web/dist。
func FromFS(fsys fs.FS) *Assets {
	a := &Assets{fsys: fsys}
	if fsys == nil {
		a.reason = "未提供资源文件系统"
		return a
	}
	info, err := fs.Stat(fsys, indexFile)
	switch {
	case err != nil:
		a.reason = reasonNotBuilt
	case info.IsDir() || info.Size() == 0:
		a.reason = "index.html 为空，前端产物不完整；请重新执行 scripts/build.sh"
	default:
		a.available = true
	}
	return a
}

// Available 表示是否可以直接对外提供页面。
func (a *Assets) Available() bool { return a != nil && a.available }

// Reason 返回不可用原因，用于启动日志与 503 文案。
func (a *Assets) Reason() string {
	if a == nil {
		return reasonNotBuilt
	}
	return a.reason
}

// Options 是构造静态资源处理器的参数。
type Options struct {
	// Logger 必须非空：资源缺失、注入失败都要有记录。
	Logger *slog.Logger
	// Mode 为运行模式（production / mock），注入到页面的 window.__UPANEL_MODE__。
	Mode string
}

// Handler 提供 SPA 静态服务。
type Handler struct {
	logger    *slog.Logger
	mode      string
	assets    *Assets
	index     []byte
	available bool
	reason    string
	modTime   time.Time
}

// NewHandler 读取入口页并构造处理器。含前端产物的二进制不需要额外参数。
func NewHandler(assets *Assets, opts Options) (*Handler, error) {
	if opts.Logger == nil {
		return nil, errors.New("webui: Logger 不能为空")
	}
	if assets == nil {
		return nil, errors.New("webui: Assets 不能为空")
	}
	mode := strings.TrimSpace(opts.Mode)
	if mode == "" {
		mode = "production"
	}

	h := &Handler{
		logger:    opts.Logger,
		mode:      mode,
		assets:    assets,
		available: assets.Available(),
		reason:    assets.Reason(),
		modTime:   time.Now().UTC(),
	}
	if h.available {
		index, err := fs.ReadFile(assets.fsys, indexFile)
		if err != nil {
			return nil, fmt.Errorf("webui: 读取 %s 失败: %w", indexFile, err)
		}
		h.index = index
	}
	return h, nil
}

// ServeHTTP 实现静态文件服务 + SPA 回退。
//
// 行为约定：
//   - 只接受 GET/HEAD，其他方法 405 并带 Allow（不把写请求当静态请求）；
//   - 命中真实文件就按扩展名给 Content-Type，带缓存策略；
//   - 未命中且“看起来像文件”（带扩展名）→ 404，**不**回退到 index.html，
//     避免把 /assets/x.js 变成一个 200 的 HTML；
//   - 未命中且是页面路径 → 返回 index.html 由前端路由处理（前端自己渲染 404 页面）；
//   - 前端产物缺失 → 503 + 明确说明，绝不返回一个看起来正常的页面。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setBaseHeaders(w)

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeText(w, r, http.StatusMethodNotAllowed, "静态资源只支持 GET 与 HEAD")
		return
	}

	if !h.available {
		writeText(w, r, http.StatusServiceUnavailable, h.reason)
		return
	}

	name, ok := cleanRequestPath(r.URL.Path)
	if !ok {
		writeText(w, r, http.StatusNotFound, "资源不存在")
		return
	}

	if name == "" || name == indexFile {
		h.serveIndex(w, r)
		return
	}

	data, err := fs.ReadFile(h.assets.fsys, name)
	if err == nil {
		h.serveAsset(w, r, name, data)
		return
	}
	if !errors.Is(err, fs.ErrNotExist) {
		h.logger.Error("读取前端资源失败", "path", name, "error", err)
		writeText(w, r, http.StatusInternalServerError, "读取前端资源失败")
		return
	}

	// 带扩展名的路径按“请求某个文件”处理：回退到 index.html 只会产生错误的 200。
	if path.Ext(name) != "" {
		writeText(w, r, http.StatusNotFound, "资源不存在")
		return
	}
	h.serveIndex(w, r)
}

// cleanRequestPath 把请求路径规范化为资源键（相对 dist 根）。
//
// 返回 ok=false 表示路径本身可疑（含空字节或清理后仍越界），直接 404。
func cleanRequestPath(urlPath string) (string, bool) {
	if strings.ContainsRune(urlPath, 0) {
		return "", false
	}
	// path.Clean 会消掉 ".."；再裁掉前导 "/" 得到相对键。
	cleaned := path.Clean("/" + urlPath)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return "", true
	}
	if strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	if contentType := contentTypeFor(name); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	// 构建产物带内容哈希，可以长期缓存；其余资源走短缓存。
	if isHashedAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, path.Base(name), h.modTime, bytes.NewReader(data))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	nonce, err := newNonce()
	if err != nil {
		h.logger.Error("生成 CSP nonce 失败", "error", err)
		writeText(w, r, http.StatusInternalServerError, "服务内部错误")
		return
	}
	page, err := h.renderIndex(nonce)
	if err != nil {
		h.logger.Error("渲染入口页失败", "error", err, "mode", h.mode)
		writeText(w, r, http.StatusInternalServerError, "入口页不可用："+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 入口页包含服务端注入的运行模式，不允许缓存，避免 mock/production 串台。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy(nonce))
	http.ServeContent(w, r, indexFile, h.modTime, bytes.NewReader(page))
}

// renderIndex 在 </head> 之前注入运行模式脚本。
func (h *Handler) renderIndex(nonce string) ([]byte, error) {
	marker := []byte("</head>")
	idx := bytes.Index(h.index, marker)
	if idx < 0 {
		return nil, errors.New("index.html 缺少 </head>，无法注入运行模式")
	}

	modeLiteral, err := json.Marshal(h.mode)
	if err != nil {
		return nil, fmt.Errorf("序列化运行模式失败: %w", err)
	}
	// json.Marshal 会把 <>& 转义成 < 等，即使模式值被污染也无法闭合 <script>。
	script := fmt.Sprintf(`<script nonce=%s>window.__UPANEL_MODE__=%s;</script>`,
		strconv.Quote(nonce), modeLiteral)

	out := make([]byte, 0, len(h.index)+len(script))
	out = append(out, h.index[:idx]...)
	out = append(out, script...)
	out = append(out, h.index[idx:]...)
	return out, nil
}

// setBaseHeaders 设置所有静态响应共享的安全头。
func setBaseHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	// 面板不允许被任何页面嵌入，避免点击劫持（PLAN.md 第 10 节：最小暴露）。
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// contentSecurityPolicy 只允许加载本二进制内的资源；inline 脚本必须带本次响应的 nonce。
//
// style-src 保留 'unsafe-inline'：Naive UI 在运行时注入 <style>，这是组件库的实现方式，
// 在不改技术栈的前提下无法用 nonce 覆盖。
func contentSecurityPolicy(nonce string) string {
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'nonce-" + nonce + "'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func newNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// contentTypeFor 先查固定表，再退回系统 MIME 表。
//
// 不直接依赖 mime.TypeByExtension：开发宿主的系统 MIME 表与 Linux 不同（Windows 会把 .js
// 映射成别的类型），而响应头必须可预测、可测试。
func contentTypeFor(name string) string {
	if value, ok := fixedContentTypes[strings.ToLower(path.Ext(name))]; ok {
		return value
	}
	return mime.TypeByExtension(path.Ext(name))
}

var fixedContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".ico":   "image/x-icon",
	".webp":  "image/webp",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".txt":   "text/plain; charset=utf-8",
}

// isHashedAsset 判断是否是带内容哈希的构建产物（/assets/index-<hash>.js）。
func isHashedAsset(name string) bool {
	dir := path.Dir(name)
	if dir != "assets" {
		return false
	}
	base := path.Base(name)
	dash := strings.LastIndex(base, "-")
	if dash < 0 {
		return false
	}
	hash := strings.TrimSuffix(base[dash+1:], path.Ext(base))
	return len(hash) >= 8
}

// writeText 写纯文本响应。静态路径的错误不用 JSON 信封：
// 这些请求来自浏览器地址栏或资源加载，不是 API 客户端。
func writeText(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", message)
}
