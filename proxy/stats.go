package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

// failKind 是请求结果的分类，统计、日志和 SOCKS5 应答码共用。
type failKind int

const (
	kindOK       failKind = iota
	kindAuth              // 用户名或密码错误
	kindRejected          // 用户名后缀不合法，或者指定的出口地址不在池内
	kindNoIPv6            // 目标没有 IPv6 地址，或者 DNS 解析失败
	kindRefused           // 目标拒绝连接
	kindTimeout           // 连接目标超时
	kindOther
	numKinds
)

var kindNames = [numKinds]string{"ok", "auth", "rejected", "no_ipv6", "refused", "timeout", "other"}

func (k failKind) String() string { return kindNames[k] }

func classify(err error) failKind {
	switch {
	case err == nil:
		return kindOK
	case errors.Is(err, errAuth):
		return kindAuth
	case errors.Is(err, tunnel.ErrNotInPool):
		return kindRejected
	case errors.Is(err, tunnel.ErrNoIPv6):
		return kindNoIPv6
	// gonet 把 tcpip 错误转成了纯文本，只能按文本匹配
	case strings.Contains(err.Error(), "refused"):
		return kindRefused
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out"):
		return kindTimeout
	}
	return kindOther
}

// Stats 是代理启动以来的累计统计。
type Stats struct {
	Count    [numKinds]uint64 // 按结果分类的请求数
	Active   int64            // 当前正在转发的连接数
	Up, Down uint64           // 客户端发往目标、目标发回客户端的字节数
}

// Requests 返回请求总数。
func (s Stats) Requests() uint64 {
	var n uint64
	for _, c := range s.Count {
		n += c
	}
	return n
}

// Sub 返回 s 相对 prev 的增量；Active 是当前值，不做减法。
func (s Stats) Sub(prev Stats) Stats {
	for k := range s.Count {
		s.Count[k] -= prev.Count[k]
	}
	s.Up -= prev.Up
	s.Down -= prev.Down
	return s
}

// Attrs 返回用于 slog 的键值对，只列出非零的分类。
func (s Stats) Attrs() []any {
	a := []any{"requests", s.Requests()}
	for k, n := range s.Count {
		if n > 0 {
			a = append(a, failKind(k).String(), n)
		}
	}
	return append(a, "active", s.Active, "up", size(s.Up), "down", size(s.Down))
}

func size(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f%ciB", v, "KMGTP"[i])
}

type stats struct {
	count    [numKinds]atomic.Uint64
	active   atomic.Int64
	up, down atomic.Uint64
}

// Stats 返回累计统计的快照。
func (p *Proxy) Stats() Stats {
	var s Stats
	for k := range s.Count {
		s.Count[k] = p.stats.count[k].Load()
	}
	s.Active = p.stats.active.Load()
	s.Up, s.Down = p.stats.up.Load(), p.stats.down.Load()
	return s
}

// request 记录一次代理请求。客户端提交了认证信息（或者无需认证）之后才产生，
// 握手阶段的协议错误（例如端口扫描）不计入。
type request struct {
	proto    string // socks5、socks5-udp、http-connect、http
	client   string
	user     string // 完整用户名（含后缀），不记录密码
	dst      string
	src      netip.Addr // 实际使用的出口地址
	start    time.Time
	up, down int64
}

func newRequest(proto, client string) *request {
	return &request{proto: proto, client: client, start: time.Now()}
}

// finish 计入统计，并在 debug 级别输出一行。
func (p *Proxy) finish(r *request, err error) {
	k := classify(err)
	p.stats.count[k].Add(1)
	p.stats.up.Add(uint64(r.up))
	p.stats.down.Add(uint64(r.down))
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	attrs := []any{"proto", r.proto, "client", r.client, "user", r.user, "dst", r.dst}
	if r.src.IsValid() {
		attrs = append(attrs, "src", r.src)
	}
	attrs = append(attrs, "dur", time.Since(r.start).Round(time.Millisecond), "up", r.up, "down", r.down)
	if err != nil {
		slog.Debug("请求失败", append(attrs, "result", k.String(), "err", err)...)
		return
	}
	slog.Debug("请求完成", attrs...)
}

// active 在转发期间计入活跃连接数，返回的函数在转发结束时调用。
func (p *Proxy) active() func() {
	p.stats.active.Add(1)
	return func() { p.stats.active.Add(-1) }
}

func localAddr(c net.Conn) netip.Addr {
	ap, _ := netip.ParseAddrPort(c.LocalAddr().String())
	return ap.Addr()
}

// countConn 统计上游连接的读写字节数，用于 HTTP 普通转发的流量统计。
type countConn struct {
	net.Conn
	up, down *atomic.Int64
}

func (c countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.down.Add(int64(n))
	return n, err
}

func (c countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.up.Add(int64(n))
	return n, err
}
