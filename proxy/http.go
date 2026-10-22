package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

// ServeHTTP 实现 HTTP 代理：CONNECT 隧道，以及 absolute-URI 形式的普通 http:// 请求转发。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 借用标准库的 Basic 解析：把 Proxy-Authorization 当作 Authorization
	user, pass, _ := (&http.Request{Header: http.Header{"Authorization": r.Header.Values("Proxy-Authorization")}}).BasicAuth()
	src, err := p.source(user, pass)
	if errors.Is(err, errAuth) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="he-ipv6-pool"`)
		http.Error(w, "需要代理认证", http.StatusProxyAuthRequired)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		p.httpConnect(w, r, src)
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "这是代理服务器，请求需要使用 absolute-URI", http.StatusBadRequest)
		return
	}
	p.forward(w, r, src)
}

func (p *Proxy) httpConnect(w http.ResponseWriter, r *http.Request, src netip.Addr) {
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	up, err := p.Tunnel.Dial(ctx, src, "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), dialStatus(err))
		return
	}
	defer up.Close()
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer c.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// 客户端可能在收到 200 之前就发了数据，已被 bufio 读走的部分先转给上游
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	relay(c, up)
}

// forward 转发普通 http:// 请求。每个请求新建上游连接（DisableKeepAlives），
// 避免复用以其他源地址建立的连接。
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, src netip.Addr) {
	rp := &httputil.ReverseProxy{
		// 请求本身就是 absolute-URI，无需改写；不调用 SetXForwarded，不暴露客户端 IP
		Rewrite: func(*httputil.ProxyRequest) {},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return p.Tunnel.Dial(ctx, src, network, addr)
			},
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, err.Error(), dialStatus(err))
		},
	}
	rp.ServeHTTP(w, r)
}

// dialStatus 把拨号错误映射成 HTTP 状态码。
func dialStatus(err error) int {
	if errors.Is(err, tunnel.ErrNotInPool) {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}
