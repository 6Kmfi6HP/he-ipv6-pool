package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"sync/atomic"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

// ServeHTTP 实现 HTTP 代理：CONNECT 隧道，以及 absolute-URI 形式的普通 http:// 请求转发。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 借用标准库的 Basic 解析：把 Proxy-Authorization 当作 Authorization
	user, pass, _ := (&http.Request{Header: http.Header{"Authorization": r.Header.Values("Proxy-Authorization")}}).BasicAuth()
	rec := newRequest("http", r.RemoteAddr)
	rec.user, rec.dst = user, r.Host
	src, err := p.source(user, pass)
	switch {
	case errors.Is(err, errAuth):
		w.Header().Set("Proxy-Authenticate", `Basic realm="he-ipv6-pool"`)
		http.Error(w, "需要代理认证", http.StatusProxyAuthRequired)
	case err != nil:
		http.Error(w, err.Error(), http.StatusForbidden)
	case r.Method == http.MethodConnect:
		rec.proto = "http-connect"
		err = p.httpConnect(w, r, src, rec)
	case !r.URL.IsAbs():
		err = errors.New("不是 absolute-URI 形式的代理请求")
		http.Error(w, "这是代理服务器，请求需要使用 absolute-URI", http.StatusBadRequest)
	default:
		err = p.forward(w, r, src, rec)
	}
	p.finish(rec, err)
}

func (p *Proxy) httpConnect(w http.ResponseWriter, r *http.Request, src netip.Addr, rec *request) error {
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	up, err := p.Tunnel.Dial(ctx, src, "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), dialStatus(err))
		return err
	}
	defer up.Close()
	rec.src = localAddr(up)
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	defer c.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return err
	}
	// 客户端可能在收到 200 之前就发了数据，已被 bufio 读走的部分先转给上游
	var early int64
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			return err
		}
		early = int64(n)
	}
	defer p.active()()
	rec.up, rec.down = relay(c, up)
	rec.up += early
	return nil
}

// forward 转发普通 http:// 请求。每个请求新建上游连接（DisableKeepAlives），
// 避免复用以其他源地址建立的连接。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, src netip.Addr, rec *request) error {
	var up, down atomic.Int64
	var ferr error
	rp := &httputil.ReverseProxy{
		// 请求本身就是 absolute-URI，无需改写；不调用 SetXForwarded，不暴露客户端 IP
		Rewrite: func(*httputil.ProxyRequest) {},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := p.Tunnel.Dial(ctx, src, network, addr)
				if err != nil {
					return nil, err
				}
				rec.src = localAddr(c)
				return countConn{Conn: c, up: &up, down: &down}, nil
			},
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			ferr = err
			http.Error(w, err.Error(), dialStatus(err))
		},
	}
	defer p.active()()
	rp.ServeHTTP(w, r)
	rec.up, rec.down = up.Load(), down.Load()
	return ferr
}

// dialStatus 把拨号错误映射成 HTTP 状态码。
func dialStatus(err error) int {
	if errors.Is(err, tunnel.ErrNotInPool) {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}
