package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Serve 在 l 上同时提供 SOCKS5 和 HTTP 代理：读取每个连接的首字节，0x05 交给 SOCKS5，
// 其余（HTTP 请求总是以 ASCII 方法名开头）交给内部的 http.Server。直到 l 关闭才返回；
// l 关闭之外的 Accept 错误（例如 EMFILE 之类的瞬时错误）只记录警告并继续服务。
func (p *Proxy) Serve(l net.Listener) error {
	hl := &chanListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: l.Addr()}
	defer hl.Close()
	go (&http.Server{Handler: p, ReadHeaderTimeout: dialTimeout}).Serve(hl)
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			slog.Warn("accept 失败", "err", err)
			time.Sleep(100 * time.Millisecond) // ponytail: 固定 100ms 退避，需要时再改指数退避
			continue
		}
		go p.dispatch(c, hl)
	}
}

// dispatch 按首字节把连接分给 SOCKS5 或 HTTP。
func (p *Proxy) dispatch(c net.Conn, hl *chanListener) {
	var first [1]byte
	c.SetReadDeadline(time.Now().Add(dialTimeout))
	if _, err := io.ReadFull(c, first[:]); err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	pc := &peekConn{Conn: c, r: io.MultiReader(bytes.NewReader(first[:]), c)}
	if first[0] == socksVer {
		defer pc.Close()
		if err := p.socks5(pc); err != nil {
			slog.Debug("socks5 握手失败", "client", c.RemoteAddr(), "err", err)
		}
		return
	}
	select {
	case hl.ch <- pc:
	case <-hl.done:
		c.Close()
	}
}

// peekConn 把分流时读走的首字节放回读取流。
type peekConn struct {
	net.Conn
	r io.Reader
}

func (c *peekConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite 透传给底层连接：嵌入接口不会提升 CloseWrite，relay 的半关闭依赖它。
func (c *peekConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// chanListener 把分流出来的 HTTP 连接交给 http.Server。
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
	addr net.Addr
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
