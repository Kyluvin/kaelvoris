package remote

// middleware.go —— HTTP 中间件统一注册点（规划板条目：internal/remote
// 统一中间件链式注册点，承接五项中间件待办）。
//
// 现状依据：Go 包内路由注册散布在 server.go / images.go / im_relay.go /
// market.go 四处 mux.HandleFunc 调用，鉴权包装（restAuth/docAuth/imageAuth）
// 在各注册点零散嵌套，逐条散改易漂移且存在顺序依赖（日志/恢复须最外层）。
//
// 两条接入路径：
//   - 全局链：s.use(mw) 注册，Start 时 Chain(s.mux, globalMW...) 包在全部
//     路由外层——先注册者最外层（请求最先经过；访问日志/panic 恢复类须最先注册）；
//   - 路由链：s.route(...) 挂载单条路由时可携带 per-route 中间件
//     （既有 restAuth/docAuth/imageAuth 鉴权即经此接入）。
//
// 后续五项中间件（访问日志、panic 恢复、Host 头白名单、Bearer Token 鉴权、
// IP 限流）落地时按位插入：全局的 s.use 一行接入，路由级的在对应 route 调用
// 追加一个实参，顺序由声明位次编排。

import "net/http"

// Middleware HTTP 中间件：包装 next 返回新 handler。
type Middleware func(http.Handler) http.Handler

// Chain 按声明顺序汇聚中间件与最终 handler：Chain(final, a, b, c) 构造为
// a(b(c(final)))——先声明者在最外层，请求依次经过 a→b→c→final。中间件列表
// 为空时原样返回 final（零包装开销，与旧版直挂行为完全一致）。
func Chain(final http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		final = mws[i](final)
	}
	return final
}

// route 统一路由注册入口：internal/remote 全部 HTTP 路由必须经此挂载
// （NewServer 及各 register*Routes），禁止再直接 mux.HandleFunc/Handle 零散
// 注册，保证后续中间件可统一按位编排。mws 为该路由的 per-route 中间件
// （先声明者靠外，包裹最终 handler h）。
func (s *Server) route(mux *http.ServeMux, pattern string, h http.HandlerFunc, mws ...Middleware) {
	mux.Handle(pattern, Chain(h, mws...))
}

// use 注册全局中间件（NewServer 之后、Start 之前调用）：包在全部路由外层，
// 先注册者最外层（请求最先经过）。零中间件时 Start 行为与旧版逐字节一致。
func (s *Server) use(mws ...Middleware) {
	s.mu.Lock()
	s.globalMW = append(s.globalMW, mws...)
	s.mu.Unlock()
}
