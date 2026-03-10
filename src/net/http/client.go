// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// HTTP client. See RFC 7230 through 7235.
//
// This is the high-level Client interface.
// The low-level implementation is in transport.go.

package http

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/internal/ascii"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client 是一个 HTTP 客户端。其零值（[DefaultClient]）是一个可用的客户端，
// 使用 [DefaultTransport] 作为底层传输层。
//
// [Client.Transport] 通常包含内部状态（如缓存的 TCP 连接），
// 因此 Client 应该被复用而不是按需创建。
// Client 对于多个 goroutine 并发使用是安全的。
//
// Client 比 [RoundTripper]（如 [Transport]）更高层，
// 还额外处理了 Cookie 和重定向等 HTTP 细节。
//
// 在跟随重定向时，Client 会转发初始 [Request] 上设置的所有头部，但以下情况除外：
//
//   - 向不受信任的目标转发敏感头部时，如 "Authorization"、
//     "WWW-Authenticate" 和 "Cookie"。
//     当重定向到的域名既不是初始域名的子域名也不完全匹配时，
//     这些头部将被忽略。
//     例如，从 "foo.com" 重定向到 "foo.com" 或 "sub.foo.com" 会转发敏感头部，
//     但重定向到 "bar.com" 则不会。
//   - 在使用非 nil 的 cookie Jar 转发 "Cookie" 头部时。
//     由于每次重定向都可能改变 cookie jar 的状态，
//     重定向可能会修改初始请求中设置的 cookie。
//     在转发 "Cookie" 头部时，已被修改的 cookie 将被省略，
//     期望由 Jar 以更新后的值重新插入这些 cookie（前提是 origin 匹配）。
//     如果 Jar 为 nil，则初始 cookie 不经修改地直接转发。
type Client struct {
	// Transport 指定发送单个 HTTP 请求所使用的底层机制。
	// 如果为 nil，则使用 DefaultTransport。
	Transport RoundTripper

	// CheckRedirect 指定处理重定向的策略。
	// 如果 CheckRedirect 非 nil，客户端在跟随 HTTP 重定向之前会调用它。
	// 参数 req 是即将发送的请求，via 是已发送的请求列表（按时间从早到晚排列）。
	// 如果 CheckRedirect 返回错误，Client 的 Get 方法将同时返回
	// 前一个 Response（其 Body 已关闭）和 CheckRedirect 的错误
	// （包装在 url.Error 中），而不会发送请求 req。
	// 特殊情况：如果 CheckRedirect 返回 ErrUseLastResponse，
	// 则返回最近一次的响应（Body 未关闭）以及 nil 错误。
	//
	// 如果 CheckRedirect 为 nil，Client 使用默认策略：
	// 连续重定向超过 10 次后停止。
	CheckRedirect func(req *Request, via []*Request) error

	// Jar 指定 cookie jar（cookie 存储器）。
	//
	// Jar 用于向每个出站请求插入相关的 cookie，
	// 并根据每个入站响应的 cookie 值进行更新。
	// 每次 Client 跟随重定向时都会查询 Jar。
	//
	// 如果 Jar 为 nil，则仅当 Request 上显式设置了 cookie 时才会发送。
	Jar CookieJar

	// Timeout 指定此 Client 发出的请求的超时时间限制。
	// 超时时间包括连接时间、所有重定向时间以及读取响应体的时间。
	// 计时器在 Get、Head、Post 或 Do 返回后仍继续运行，
	// 并会中断对 Response.Body 的读取。
	//
	// Timeout 为零表示不设超时。
	//
	// Client 会像 Request 的 Context 结束一样，
	// 取消底层 Transport 上的请求。
	//
	// 为了兼容性，如果 Transport 上存在已废弃的 CancelRequest 方法，
	// Client 也会使用它。新的 RoundTripper 实现应使用 Request 的 Context
	// 来实现取消，而不是实现 CancelRequest。
	Timeout time.Duration
}

// DefaultClient 是默认的 [Client]，供 [Get]、[Head] 和 [Post] 使用。
var DefaultClient = &Client{}

// RoundTripper is an interface representing the ability to execute a
// single HTTP transaction, obtaining the [Response] for a given [Request].
//
// A RoundTripper must be safe for concurrent use by multiple
// goroutines.
//
// [译] RoundTripper 是一个接口，代表执行单次 HTTP 事务的能力，即为给定的 Request 获取对应的 Response。
// 必须能被多个 goroutine 并发安全地使用。
// 核心职责：只负责底层网络传输（一次请求 → 一次响应的"往返"），
// 不处理重定向、认证、Cookie 等高层逻辑（那些由 http.Client 负责）。
// 常见用途：包装 http.DefaultTransport 实现中间件，如自动注入鉴权 Header、日志、限流、重试等。
type RoundTripper interface {
	// RoundTrip executes a single HTTP transaction, returning
	// a Response for the provided Request.
	//
	// RoundTrip should not attempt to interpret the response. In
	// particular, RoundTrip must return err == nil if it obtained
	// a response, regardless of the response's HTTP status code.
	// A non-nil err should be reserved for failure to obtain a
	// response. Similarly, RoundTrip should not attempt to
	// handle higher-level protocol details such as redirects,
	// authentication, or cookies.
	//
	// RoundTrip should not modify the request, except for
	// consuming and closing the Request's Body. RoundTrip may
	// read fields of the request in a separate goroutine. Callers
	// should not mutate or reuse the request until the Response's
	// Body has been closed.
	//
	// RoundTrip must always close the body, including on errors,
	// but depending on the implementation may do so in a separate
	// goroutine even after RoundTrip returns. This means that
	// callers wanting to reuse the body for subsequent requests
	// must arrange to wait for the Close call before doing so.
	//
	// The Request's URL and Header fields must be initialized.
	//
	// [译] RoundTrip 执行单次 HTTP 事务，为 Request 返回对应的 Response。
	// 错误语义：err != nil 仅代表"网络失败，未能获取响应"；HTTP 4xx/5xx 状态码不算错误（err 仍为 nil）。
	// 不应处理重定向、认证、Cookie 等高层协议细节。
	// 不应修改请求（唯一例外：可以消费并关闭 Request.Body）。
	// 可能在另一个 goroutine 中读取请求字段，因此调用者在 Response.Body 关闭前不得修改或复用请求。
	// 必须始终关闭请求 Body（含出错时），防止连接泄漏；但关闭操作可能发生在 RoundTrip 返回之后的另一个 goroutine 中。
	// 调用前必须确保 Request 的 URL 和 Header 字段已初始化。
	RoundTrip(*Request) (*Response, error)
}

// refererForURL returns a referer without any authentication info or
// an empty string if lastReq scheme is https and newReq scheme is http.
// If the referer was explicitly set, then it will continue to be used.
func refererForURL(lastReq, newReq *url.URL, explicitRef string) string {
	// https://tools.ietf.org/html/rfc7231#section-5.5.2
	//   "Clients SHOULD NOT include a Referer header field in a
	//    (non-secure) HTTP request if the referring page was
	//    transferred with a secure protocol."
	if lastReq.Scheme == "https" && newReq.Scheme == "http" {
		return ""
	}
	if explicitRef != "" {
		return explicitRef
	}

	referer := lastReq.String()
	if lastReq.User != nil {
		// This is not very efficient, but is the best we can
		// do without:
		// - introducing a new method on URL
		// - creating a race condition
		// - copying the URL struct manually, which would cause
		//   maintenance problems down the line
		auth := lastReq.User.String() + "@"
		referer = strings.Replace(referer, auth, "", 1)
	}
	return referer
}

// didTimeout is non-nil only if err != nil.
func (c *Client) send(req *Request, deadline time.Time) (resp *Response, didTimeout func() bool, err error) {
	if c.Jar != nil {
		for _, cookie := range c.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	resp, didTimeout, err = send(req, c.transport(), deadline)
	if err != nil {
		return nil, didTimeout, err
	}
	if c.Jar != nil {
		if rc := resp.Cookies(); len(rc) > 0 {
			c.Jar.SetCookies(req.URL, rc)
		}
	}
	return resp, nil, nil
}

func (c *Client) deadline() time.Time {
	if c.Timeout > 0 {
		return time.Now().Add(c.Timeout)
	}
	return time.Time{}
}

func (c *Client) transport() RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return DefaultTransport
}

// ErrSchemeMismatch 在服务器向 HTTPS 客户端返回 HTTP 响应时返回此错误。
var ErrSchemeMismatch = errors.New("http: server gave HTTP response to HTTPS client")

// send 发送一个 HTTP 请求。
// 调用方在读取完毕后应关闭 resp.Body。
func send(ireq *Request, rt RoundTripper, deadline time.Time) (resp *Response, didTimeout func() bool, err error) {
	req := ireq // req 要么是原始请求，要么是经过修改的浅拷贝副本

	if rt == nil {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: no Client.Transport or DefaultTransport")
	}

	if req.URL == nil {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: nil Request.URL")
	}

	if req.RequestURI != "" {
		req.closeBody()
		return nil, alwaysFalse, errors.New("http: Request.RequestURI can't be set in client requests")
	}

	// forkReq 在第一次调用时将 req 浅拷贝为 ireq 的副本，
	// 避免修改调用方传入的原始请求。
	forkReq := func() {
		if ireq == req {
			req = new(Request)
			*req = *ireq // 浅拷贝
		}
	}

	// send 的大多数调用方（Get、Post 等）不需要设置 Header，
	// 因此 Header 可能未被初始化。
	// 但我们需要向 Transport 保证 Header 已初始化。
	if req.Header == nil {
		forkReq()
		req.Header = make(Header)
	}

	// 如果 URL 中包含用户名/密码信息，且请求头中尚未设置 Authorization，
	// 则自动提取并添加 HTTP Basic 认证头。
	if u := req.URL.User; u != nil && req.Header.Get("Authorization") == "" {
		username := u.Username()
		password, _ := u.Password()
		forkReq()
		req.Header = cloneOrMakeHeader(ireq.Header)
		req.Header.Set("Authorization", "Basic "+basicAuth(username, password))
	}

	// 如果设置了截止时间，需要 fork 请求以便安全地附加取消逻辑
	if !deadline.IsZero() {
		forkReq()
	}
	// 设置请求取消机制，返回停止计时器的函数和判断是否超时的函数
	stopTimer, didTimeout := setRequestCancel(req, rt, deadline)

	// 通过 Transport 发送请求
	resp, err = rt.RoundTrip(req)
	if err != nil {
		stopTimer()
		if resp != nil {
			log.Printf("RoundTripper returned a response & error; ignoring response")
		}
		if tlsErr, ok := err.(tls.RecordHeaderError); ok {
			// 如果收到非法的 TLS 记录头，检查响应是否像 HTTP，
			// 若是则返回更具描述性的协议不匹配错误。
			// 参见 golang.org/issue/11111。
			if string(tlsErr.RecordHeader[:]) == "HTTP/" {
				err = ErrSchemeMismatch
			}
		}
		return nil, didTimeout, err
	}
	// RoundTripper 不应在无错误时返回 nil Response
	if resp == nil {
		return nil, didTimeout, fmt.Errorf("http: RoundTripper implementation (%T) returned a nil *Response with a nil error", rt)
	}
	if resp.Body == nil {
		// Body 字段的文档说明："http Client 和 Transport 保证 Body 始终非 nil，
		// 即使响应没有 body 或 body 长度为零也是如此。"
		// 但我们没有对任意 RoundTripper 实现施加同样的约束，
		// 实际中（主要是在测试里）的 RoundTripper 实现假设可以用 nil Body 表示空 body
		// （类似于 Request.Body 的语义）。
		// 参见 https://golang.org/issue/38095。
		//
		// 如果 ContentLength 允许 Body 为空，则在此处填入一个空 Body
		// 以确保其非 nil。
		if resp.ContentLength > 0 && req.Method != "HEAD" {
			return nil, didTimeout, fmt.Errorf("http: RoundTripper implementation (%T) returned a *Response with content length %d but a nil Body", rt, resp.ContentLength)
		}
		resp.Body = io.NopCloser(strings.NewReader(""))
	}
	// 如果设置了截止时间，将 resp.Body 包装为带超时取消能力的 cancelTimerBody
	if !deadline.IsZero() {
		resp.Body = &cancelTimerBody{
			stop:          stopTimer,
			rc:            resp.Body,
			reqDidTimeout: didTimeout,
		}
	}
	return resp, nil, nil
}

// timeBeforeContextDeadline reports whether the non-zero Time t is
// before ctx's deadline, if any. If ctx does not have a deadline, it
// always reports true (the deadline is considered infinite).
func timeBeforeContextDeadline(t time.Time, ctx context.Context) bool {
	d, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return t.Before(d)
}

// knownRoundTripperImpl reports whether rt is a RoundTripper that's
// maintained by the Go team and known to implement the latest
// optional semantics (notably contexts). The Request is used
// to check whether this particular request is using an alternate protocol,
// in which case we need to check the RoundTripper for that protocol.
func knownRoundTripperImpl(rt RoundTripper, req *Request) bool {
	switch t := rt.(type) {
	case *Transport:
		if altRT := t.alternateRoundTripper(req); altRT != nil {
			return knownRoundTripperImpl(altRT, req)
		}
		return true
	case *http2Transport, http2noDialH2RoundTripper:
		return true
	}
	// There's a very minor chance of a false positive with this.
	// Instead of detecting our golang.org/x/net/http2.Transport,
	// it might detect a Transport type in a different http2
	// package. But I know of none, and the only problem would be
	// some temporarily leaked goroutines if the transport didn't
	// support contexts. So this is a good enough heuristic:
	if reflect.TypeOf(rt).String() == "*http2.Transport" {
		return true
	}
	return false
}

// setRequestCancel sets req.Cancel and adds a deadline context to req
// if deadline is non-zero. The RoundTripper's type is used to
// determine whether the legacy CancelRequest behavior should be used.
//
// As background, there are three ways to cancel a request:
// First was Transport.CancelRequest. (deprecated)
// Second was Request.Cancel.
// Third was Request.Context.
// This function populates the second and third, and uses the first if it really needs to.
func setRequestCancel(req *Request, rt RoundTripper, deadline time.Time) (stopTimer func(), didTimeout func() bool) {
	if deadline.IsZero() {
		return nop, alwaysFalse
	}
	knownTransport := knownRoundTripperImpl(rt, req)
	oldCtx := req.Context()

	if req.Cancel == nil && knownTransport {
		// If they already had a Request.Context that's
		// expiring sooner, do nothing:
		if !timeBeforeContextDeadline(deadline, oldCtx) {
			return nop, alwaysFalse
		}

		var cancelCtx func()
		req.ctx, cancelCtx = context.WithDeadline(oldCtx, deadline)
		return cancelCtx, func() bool { return time.Now().After(deadline) }
	}
	initialReqCancel := req.Cancel // the user's original Request.Cancel, if any

	var cancelCtx func()
	if timeBeforeContextDeadline(deadline, oldCtx) {
		req.ctx, cancelCtx = context.WithDeadline(oldCtx, deadline)
	}

	cancel := make(chan struct{})
	req.Cancel = cancel

	doCancel := func() {
		// The second way in the func comment above:
		close(cancel)
		// The first way, used only for RoundTripper
		// implementations written before Go 1.5 or Go 1.6.
		type canceler interface{ CancelRequest(*Request) }
		if v, ok := rt.(canceler); ok {
			v.CancelRequest(req)
		}
	}

	stopTimerCh := make(chan struct{})
	stopTimer = sync.OnceFunc(func() {
		close(stopTimerCh)
		if cancelCtx != nil {
			cancelCtx()
		}
	})

	timer := time.NewTimer(time.Until(deadline))
	var timedOut atomic.Bool

	go func() {
		select {
		case <-initialReqCancel:
			doCancel()
			timer.Stop()
		case <-timer.C:
			timedOut.Store(true)
			doCancel()
		case <-stopTimerCh:
			timer.Stop()
		}
	}()

	return stopTimer, timedOut.Load
}

// See 2 (end of page 4) https://www.ietf.org/rfc/rfc2617.txt
// "To receive authorization, the client sends the userid and password,
// separated by a single colon (":") character, within a base64
// encoded string in the credentials."
// It is not meant to be urlencoded.
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

// Get issues a GET to the specified URL. If the response is one of
// the following redirect codes, Get follows the redirect, up to a
// maximum of 10 redirects:
//
//	301 (Moved Permanently)
//	302 (Found)
//	303 (See Other)
//	307 (Temporary Redirect)
//	308 (Permanent Redirect)
//
// An error is returned if there were too many redirects or if there
// was an HTTP protocol error. A non-2xx response doesn't cause an
// error. Any returned error will be of type [*url.Error]. The url.Error
// value's Timeout method will report true if the request timed out.
//
// When err is nil, resp always contains a non-nil resp.Body.
// Caller should close resp.Body when done reading from it.
//
// Get is a wrapper around DefaultClient.Get.
//
// To make a request with custom headers, use [NewRequest] and
// DefaultClient.Do.
//
// To make a request with a specified context.Context, use [NewRequestWithContext]
// and DefaultClient.Do.
func Get(url string) (resp *Response, err error) {
	return DefaultClient.Get(url)
}

// Get issues a GET to the specified URL. If the response is one of the
// following redirect codes, Get follows the redirect after calling the
// [Client.CheckRedirect] function:
//
//	301 (Moved Permanently)
//	302 (Found)
//	303 (See Other)
//	307 (Temporary Redirect)
//	308 (Permanent Redirect)
//
// An error is returned if the [Client.CheckRedirect] function fails
// or if there was an HTTP protocol error. A non-2xx response doesn't
// cause an error. Any returned error will be of type [*url.Error]. The
// url.Error value's Timeout method will report true if the request
// timed out.
//
// When err is nil, resp always contains a non-nil resp.Body.
// Caller should close resp.Body when done reading from it.
//
// To make a request with custom headers, use [NewRequest] and [Client.Do].
//
// To make a request with a specified context.Context, use [NewRequestWithContext]
// and Client.Do.
func (c *Client) Get(url string) (resp *Response, err error) {
	req, err := NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

func alwaysFalse() bool { return false }

// ErrUseLastResponse can be returned by Client.CheckRedirect hooks to
// control how redirects are processed. If returned, the next request
// is not sent and the most recent response is returned with its body
// unclosed.
var ErrUseLastResponse = errors.New("net/http: use last response")

// checkRedirect calls either the user's configured CheckRedirect
// function, or the default.
func (c *Client) checkRedirect(req *Request, via []*Request) error {
	fn := c.CheckRedirect
	if fn == nil {
		fn = defaultCheckRedirect
	}
	return fn(req, via)
}

// redirectBehavior describes what should happen when the
// client encounters a 3xx status code from the server.
func redirectBehavior(reqMethod string, resp *Response, ireq *Request) (redirectMethod string, shouldRedirect, includeBody bool) {
	switch resp.StatusCode {
	case 301, 302, 303:
		redirectMethod = reqMethod
		shouldRedirect = true
		includeBody = false

		// RFC 2616 allowed automatic redirection only with GET and
		// HEAD requests. RFC 7231 lifts this restriction, but we still
		// restrict other methods to GET to maintain compatibility.
		// See Issue 18570.
		if reqMethod != "GET" && reqMethod != "HEAD" {
			redirectMethod = "GET"
		}
	case 307, 308:
		redirectMethod = reqMethod
		shouldRedirect = true
		includeBody = true

		if ireq.GetBody == nil && ireq.outgoingLength() != 0 {
			// We had a request body, and 307/308 require
			// re-sending it, but GetBody is not defined. So just
			// return this response to the user instead of an
			// error, like we did in Go 1.7 and earlier.
			shouldRedirect = false
		}
	}
	return redirectMethod, shouldRedirect, includeBody
}

// urlErrorOp returns the (*url.Error).Op value to use for the
// provided (*Request).Method value.
func urlErrorOp(method string) string {
	if method == "" {
		return "Get"
	}
	if lowerMethod, ok := ascii.ToLower(method); ok {
		return method[:1] + lowerMethod[1:]
	}
	return method
}

// Do 发送一个 HTTP 请求并返回一个 HTTP 响应，遵循 Client 上配置的策略
// （如重定向、Cookie、认证等）。
//
// 仅当客户端策略导致错误（如 CheckRedirect）或无法进行 HTTP 通信
// （如网络连接问题）时才会返回 error。非 2xx 状态码不会导致 error。
//
// 如果返回的 error 为 nil，则 [Response] 将包含一个非 nil 的 Body，
// 调用方需要负责关闭它。如果 Body 没有被读取到 EOF 并关闭，
// [Client] 底层的 [RoundTripper]（通常是 [Transport]）可能无法复用
// 到服务器的持久 TCP 连接来发送后续的 "keep-alive" 请求。
//
// 请求的 Body（如果非 nil）将由底层 Transport 关闭，即使发生错误也是如此。
// Body 可能在 Do 返回之后被异步关闭。
//
// 发生错误时，可以忽略任何 Response。只有当 CheckRedirect 失败时，
// 才会同时返回非 nil 的 Response 和非 nil 的 error，而且此时返回的
// [Response.Body] 已经被关闭。
//
// 通常应该使用 [Get]、[Post] 或 [PostForm] 而不是直接使用 Do。
//
// 如果服务器回复重定向，Client 首先使用 CheckRedirect 函数来判断
// 是否应该跟随重定向。如果允许，301、302 或 303 重定向会导致后续请求
// 使用 HTTP GET 方法（如果原始请求是 HEAD 则使用 HEAD），且不带请求体。
// 307 或 308 重定向会保留原始的 HTTP 方法和请求体，
// 前提是定义了 [Request.GetBody] 函数。
// [NewRequest] 函数会自动为常见的标准库 body 类型设置 GetBody。
//
// 任何返回的 error 都将是 [*url.Error] 类型。如果请求超时，
// url.Error 的 Timeout 方法将返回 true。
func (c *Client) Do(req *Request) (*Response, error) {
	return c.do(req)
}

// 测试钩子：用于在 Client.Do 返回结果时进行测试拦截
var testHookClientDoResult func(retres *Response, reterr error)

// do 是 Do 的内部实现，负责实际的请求发送和重定向跟随逻辑。
func (c *Client) do(req *Request) (retres *Response, reterr error) {
	// 如果设置了测试钩子，在函数返回时调用它
	if testHookClientDoResult != nil {
		defer func() { testHookClientDoResult(retres, reterr) }()
	}
	// 校验请求 URL 不为 nil
	if req.URL == nil {
		req.closeBody()
		return nil, &url.Error{
			Op:  urlErrorOp(req.Method),
			Err: errors.New("http: nil Request.URL"),
		}
	}
	_ = *c // 如果 c 为 nil 则提前 panic；参见 go.dev/issue/53521

	var (
		deadline      = c.deadline()             // 计算请求截止时间
		reqs          []*Request                 // 记录所有请求（包括重定向产生的）
		resp          *Response                  // 当前响应
		copyHeaders   = c.makeHeadersCopier(req) // 创建请求头复制器
		reqBodyClosed = false                    // 当前 req.Body 是否已关闭

		// 重定向行为相关变量：
		redirectMethod        string
		includeBody           = true  // 是否在重定向中携带请求体
		stripSensitiveHeaders = false // 是否剥离敏感头部
	)
	// uerr 将错误包装为 *url.Error 并处理 body 关闭
	uerr := func(err error) error {
		// body 可能已经被 c.send() 关闭
		if !reqBodyClosed {
			req.closeBody()
		}
		var urlStr string
		if resp != nil && resp.Request != nil {
			urlStr = stripPassword(resp.Request.URL)
		} else {
			urlStr = stripPassword(req.URL)
		}
		return &url.Error{
			Op:  urlErrorOp(reqs[0].Method),
			URL: urlStr,
			Err: err,
		}
	}
	for {
		// 除第一个请求外，构造下一跳的请求并替换 req
		if len(reqs) > 0 {
			loc := resp.Header.Get("Location")
			if loc == "" {
				// 虽然大多数 3xx 响应包含 Location，但这不是必须的，
				// 实际中已观察到不带 Location 的 3xx 响应。
				// 参见 issues #17773 和 #49281。
				return resp, nil
			}
			u, err := req.URL.Parse(loc)
			if err != nil {
				resp.closeBody()
				return nil, uerr(fmt.Errorf("failed to parse Location header %q: %v", loc, err))
			}
			host := ""
			if req.Host != "" && req.Host != req.URL.Host {
				// 如果调用方指定了自定义 Host 头且重定向位置是相对路径，
				// 则在重定向过程中保留 Host 头。参见 issue #22233。
				if u, _ := url.Parse(loc); u != nil && !u.IsAbs() {
					host = req.Host
				}
			}
			ireq := reqs[0]
			req = &Request{
				Method:   redirectMethod,
				Response: resp,
				URL:      u,
				Header:   make(Header),
				Host:     host,
				Cancel:   ireq.Cancel,
				ctx:      ireq.ctx,
			}
			// 如果需要携带请求体且原始请求定义了 GetBody，则重新获取 body
			if includeBody && ireq.GetBody != nil {
				req.Body, err = ireq.GetBody()
				if err != nil {
					resp.closeBody()
					return nil, uerr(err)
				}
				req.GetBody = ireq.GetBody
				req.ContentLength = ireq.ContentLength
			}

			// 在设置 Referer 之前先复制原始头部，
			// 以防用户在第一个请求中设置了 Referer。
			// 如果他们确实想覆盖，可以在 CheckRedirect 函数中处理。
			if !stripSensitiveHeaders && reqs[0].URL.Host != req.URL.Host {
				if !shouldCopyHeaderOnRedirect(reqs[0].URL, req.URL) {
					stripSensitiveHeaders = true
				}
			}
			copyHeaders(req, stripSensitiveHeaders)

			// 从最近一次请求的 URL 添加 Referer 头到新请求，
			// 除非是从 https 到 http 的降级
			if ref := refererForURL(reqs[len(reqs)-1].URL, req.URL, req.Header.Get("Referer")); ref != "" {
				req.Header.Set("Referer", ref)
			}
			err = c.checkRedirect(req, reqs)

			// 哨兵错误：允许用户选择上一个响应而不关闭其 body。
			// 参见 Issue 10069。
			if err == ErrUseLastResponse {
				return resp, nil
			}

			// 关闭前一个响应的 body。但先尝试读取一部分内容，
			// 如果 body 较小，底层 TCP 连接就可以被复用。
			// 不需要检查错误：如果失败，Transport 也不会复用它。
			const maxBodySlurpSize = 2 << 10
			if resp.ContentLength == -1 || resp.ContentLength <= maxBodySlurpSize {
				io.CopyN(io.Discard, resp.Body, maxBodySlurpSize)
			}
			resp.Body.Close()

			if err != nil {
				// Go 1 兼容性的特殊处理：如果 CheckRedirect 函数失败，
				// 同时返回 response 和 error。
				// 参见 https://golang.org/issue/3795
				// 此时 resp.Body 已经被关闭。
				ue := uerr(err)
				ue.(*url.Error).URL = loc
				return resp, ue
			}
		}

		reqs = append(reqs, req)
		var err error
		var didTimeout func() bool
		// 发送请求，c.send() 总是会关闭 req.Body
		if resp, didTimeout, err = c.send(req, deadline); err != nil {
			reqBodyClosed = true
			if !deadline.IsZero() && didTimeout() {
				err = &timeoutError{err.Error() + " (Client.Timeout exceeded while awaiting headers)"}
			}
			return nil, uerr(err)
		}

		// 判断是否需要重定向
		var shouldRedirect, includeBodyOnHop bool
		redirectMethod, shouldRedirect, includeBodyOnHop = redirectBehavior(req.Method, resp, reqs[0])
		if !shouldRedirect {
			return resp, nil
		}
		if !includeBodyOnHop {
			// 一旦某一跳丢弃了 body，后续就不再发送它
			// （因为我们现在处理的是一个无 body 的请求的重定向）。
			includeBody = false
		}

		req.closeBody()
	}
}

// makeHeadersCopier makes a function that copies headers from the
// initial Request, ireq. For every redirect, this function must be called
// so that it can copy headers into the upcoming Request.
func (c *Client) makeHeadersCopier(ireq *Request) func(req *Request, stripSensitiveHeaders bool) {
	// The headers to copy are from the very initial request.
	// We use a closured callback to keep a reference to these original headers.
	var (
		ireqhdr  = cloneOrMakeHeader(ireq.Header)
		icookies map[string][]*Cookie
	)
	if c.Jar != nil && ireq.Header.Get("Cookie") != "" {
		icookies = make(map[string][]*Cookie)
		for _, c := range ireq.Cookies() {
			icookies[c.Name] = append(icookies[c.Name], c)
		}
	}

	return func(req *Request, stripSensitiveHeaders bool) {
		// If Jar is present and there was some initial cookies provided
		// via the request header, then we may need to alter the initial
		// cookies as we follow redirects since each redirect may end up
		// modifying a pre-existing cookie.
		//
		// Since cookies already set in the request header do not contain
		// information about the original domain and path, the logic below
		// assumes any new set cookies override the original cookie
		// regardless of domain or path.
		//
		// See https://golang.org/issue/17494
		if c.Jar != nil && icookies != nil {
			var changed bool
			resp := req.Response // The response that caused the upcoming redirect
			for _, c := range resp.Cookies() {
				if _, ok := icookies[c.Name]; ok {
					delete(icookies, c.Name)
					changed = true
				}
			}
			if changed {
				ireqhdr.Del("Cookie")
				var ss []string
				for _, cs := range icookies {
					for _, c := range cs {
						ss = append(ss, c.Name+"="+c.Value)
					}
				}
				slices.Sort(ss) // Ensure deterministic headers
				ireqhdr.Set("Cookie", strings.Join(ss, "; "))
			}
		}

		// Copy the initial request's Header values
		// (at least the safe ones).
		for k, vv := range ireqhdr {
			sensitive := false
			switch CanonicalHeaderKey(k) {
			case "Authorization", "Www-Authenticate", "Cookie", "Cookie2",
				"Proxy-Authorization", "Proxy-Authenticate":
				sensitive = true
			}
			if !(sensitive && stripSensitiveHeaders) {
				req.Header[k] = vv
			}
		}
	}
}

func defaultCheckRedirect(req *Request, via []*Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// Post issues a POST to the specified URL.
//
// Caller should close resp.Body when done reading from it.
//
// If the provided body is an [io.Closer], it is closed after the
// request.
//
// Post is a wrapper around DefaultClient.Post.
//
// To set custom headers, use [NewRequest] and DefaultClient.Do.
//
// See the [Client.Do] method documentation for details on how redirects
// are handled.
//
// To make a request with a specified context.Context, use [NewRequestWithContext]
// and DefaultClient.Do.
func Post(url, contentType string, body io.Reader) (resp *Response, err error) {
	return DefaultClient.Post(url, contentType, body)
}

// Post issues a POST to the specified URL.
//
// Caller should close resp.Body when done reading from it.
//
// If the provided body is an [io.Closer], it is closed after the
// request.
//
// To set custom headers, use [NewRequest] and [Client.Do].
//
// To make a request with a specified context.Context, use [NewRequestWithContext]
// and [Client.Do].
//
// See the [Client.Do] method documentation for details on how redirects
// are handled.
func (c *Client) Post(url, contentType string, body io.Reader) (resp *Response, err error) {
	req, err := NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.Do(req)
}

// PostForm issues a POST to the specified URL, with data's keys and
// values URL-encoded as the request body.
//
// The Content-Type header is set to application/x-www-form-urlencoded.
// To set other headers, use [NewRequest] and DefaultClient.Do.
//
// When err is nil, resp always contains a non-nil resp.Body.
// Caller should close resp.Body when done reading from it.
//
// PostForm is a wrapper around DefaultClient.PostForm.
//
// See the [Client.Do] method documentation for details on how redirects
// are handled.
//
// To make a request with a specified [context.Context], use [NewRequestWithContext]
// and DefaultClient.Do.
func PostForm(url string, data url.Values) (resp *Response, err error) {
	return DefaultClient.PostForm(url, data)
}

// PostForm issues a POST to the specified URL,
// with data's keys and values URL-encoded as the request body.
//
// The Content-Type header is set to application/x-www-form-urlencoded.
// To set other headers, use [NewRequest] and [Client.Do].
//
// When err is nil, resp always contains a non-nil resp.Body.
// Caller should close resp.Body when done reading from it.
//
// See the [Client.Do] method documentation for details on how redirects
// are handled.
//
// To make a request with a specified context.Context, use [NewRequestWithContext]
// and Client.Do.
func (c *Client) PostForm(url string, data url.Values) (resp *Response, err error) {
	return c.Post(url, "application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
}

// Head issues a HEAD to the specified URL. If the response is one of
// the following redirect codes, Head follows the redirect, up to a
// maximum of 10 redirects:
//
//	301 (Moved Permanently)
//	302 (Found)
//	303 (See Other)
//	307 (Temporary Redirect)
//	308 (Permanent Redirect)
//
// Head is a wrapper around DefaultClient.Head.
//
// To make a request with a specified [context.Context], use [NewRequestWithContext]
// and DefaultClient.Do.
func Head(url string) (resp *Response, err error) {
	return DefaultClient.Head(url)
}

// Head issues a HEAD to the specified URL. If the response is one of the
// following redirect codes, Head follows the redirect after calling the
// [Client.CheckRedirect] function:
//
//	301 (Moved Permanently)
//	302 (Found)
//	303 (See Other)
//	307 (Temporary Redirect)
//	308 (Permanent Redirect)
//
// To make a request with a specified [context.Context], use [NewRequestWithContext]
// and [Client.Do].
func (c *Client) Head(url string) (resp *Response, err error) {
	req, err := NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// CloseIdleConnections closes any connections on its [Transport] which
// were previously connected from previous requests but are now
// sitting idle in a "keep-alive" state. It does not interrupt any
// connections currently in use.
//
// If [Client.Transport] does not have a [Client.CloseIdleConnections] method
// then this method does nothing.
func (c *Client) CloseIdleConnections() {
	type closeIdler interface {
		CloseIdleConnections()
	}
	if tr, ok := c.transport().(closeIdler); ok {
		tr.CloseIdleConnections()
	}
}

// cancelTimerBody is an io.ReadCloser that wraps rc with two features:
//  1. On Read error or close, the stop func is called.
//  2. On Read failure, if reqDidTimeout is true, the error is wrapped and
//     marked as net.Error that hit its timeout.
type cancelTimerBody struct {
	stop          func() // stops the time.Timer waiting to cancel the request
	rc            io.ReadCloser
	reqDidTimeout func() bool
}

func (b *cancelTimerBody) Read(p []byte) (n int, err error) {
	n, err = b.rc.Read(p)
	if err == nil {
		return n, nil
	}
	if err == io.EOF {
		return n, err
	}
	if b.reqDidTimeout() {
		err = &timeoutError{err.Error() + " (Client.Timeout or context cancellation while reading body)"}
	}
	return n, err
}

func (b *cancelTimerBody) Close() error {
	err := b.rc.Close()
	b.stop()
	return err
}

func shouldCopyHeaderOnRedirect(initial, dest *url.URL) bool {
	// Permit sending auth/cookie headers from "foo.com"
	// to "sub.foo.com".

	// Note that we don't send all cookies to subdomains
	// automatically. This function is only used for
	// Cookies set explicitly on the initial outgoing
	// client request. Cookies automatically added via the
	// CookieJar mechanism continue to follow each
	// cookie's scope as set by Set-Cookie. But for
	// outgoing requests with the Cookie header set
	// directly, we don't know their scope, so we assume
	// it's for *.domain.com.

	ihost := idnaASCIIFromURL(initial)
	dhost := idnaASCIIFromURL(dest)
	return isDomainOrSubdomain(dhost, ihost)
}

// isDomainOrSubdomain reports whether sub is a subdomain (or exact
// match) of the parent domain.
//
// Both domains must already be in canonical form.
func isDomainOrSubdomain(sub, parent string) bool {
	if sub == parent {
		return true
	}
	// If sub contains a :, it's probably an IPv6 address (and is definitely not a hostname).
	// Don't check the suffix in this case, to avoid matching the contents of a IPv6 zone.
	// For example, "::1%.www.example.com" is not a subdomain of "www.example.com".
	if strings.ContainsAny(sub, ":%") {
		return false
	}
	// If sub is "foo.example.com" and parent is "example.com",
	// that means sub must end in "."+parent.
	// Do it without allocating.
	if !strings.HasSuffix(sub, parent) {
		return false
	}
	return sub[len(sub)-len(parent)-1] == '.'
}

func stripPassword(u *url.URL) string {
	_, passSet := u.User.Password()
	if passSet {
		return strings.Replace(u.String(), u.User.String()+"@", u.User.Username()+":***@", 1)
	}
	return u.String()
}
