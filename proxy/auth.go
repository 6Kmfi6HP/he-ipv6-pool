// Package proxy 在 tunnel 之上提供 SOCKS5 和 HTTP 代理，由用户名里的选择器决定出口 IPv6。
package proxy

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

const dialTimeout = 30 * time.Second

var errAuth = errors.New("proxy: 认证失败")

// Proxy 持有隧道和认证信息。入口是 Serve（同一个端口提供 SOCKS5 和 HTTP），也可以单独作为 http.Handler 使用。
type Proxy struct {
	Tunnel *tunnel.Tunnel
	Users  map[string]string // 用户名 → 密码；为空时不要求认证

	sessions sessions // 带 -ttl- 或 -cidr- 的会话
}

// source 校验密码并按用户名里的后缀选出源地址（零值表示由隧道随机选）。
//
//	alice                       每条连接随机
//	alice-cidr-64               每条连接随机，但按 /64 子网轮换（SpreadAddr）
//	alice-session-<id>          固定 IP：StickyAddr 哈希映射，无状态，重启后不变
//	alice-session-<id>-ttl-10   固定 IP，从第一次使用起 10 分钟后换新
//	alice-session-<id>-cidr-64  固定 IP，按 /64 轮换分配，不同会话落在不同的 /64
//	alice-ttl-10                不带 session：整个用户共用一个 IP，每 10 分钟换新
//	alice-ip-<addr>             指定地址，addr 里的 '-' 视为 ':'（HTTP Basic 认证的用户名不能含冒号）
//
// 认证失败返回 errAuth；后缀不合法、显式 IP 无效或不在池内时返回 tunnel.ErrNotInPool。
func (p *Proxy) source(username, password string) (netip.Addr, error) {
	sel, err := parseSelector(username)
	if len(p.Users) > 0 {
		want, ok := p.Users[sel.user]
		if !ok || subtle.ConstantTimeCompare([]byte(want), []byte(password)) != 1 {
			return netip.Addr{}, errAuth
		}
	}
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %v", tunnel.ErrNotInPool, err)
	}
	pick := func() netip.Addr {
		if sel.cidr > 0 {
			return p.Tunnel.SpreadAddr(sel.cidr)
		}
		return p.Tunnel.RandomAddr()
	}
	switch {
	case sel.ip != "":
		a, err := netip.ParseAddr(strings.ReplaceAll(sel.ip, "-", ":"))
		if err != nil || !p.Tunnel.Contains(a) {
			return netip.Addr{}, tunnel.ErrNotInPool
		}
		return a, nil
	case !sel.sticky:
		if sel.cidr > 0 {
			return pick(), nil
		}
		return netip.Addr{}, nil
	case sel.ttl == 0 && sel.cidr == 0:
		return p.Tunnel.StickyAddr(sel.key()), nil
	}
	return p.sessions.get(sel.key(), sel.ttl, pick), nil
}

// selector 是用户名拆出来的基础用户名和出口选择后缀。
type selector struct {
	user    string
	session string        // -session-<id>
	ip      string        // -ip-<addr>
	ttl     time.Duration // -ttl-<时长>
	cidr    int           // -cidr-<位数>
	sticky  bool          // 带了 -session- 或 -ttl-：需要固定 IP
}

// key 是会话的标识；保持 "<user>-session-<id>" 的形式，老的 -session- 用户名映射到的 IP 不变。
func (s selector) key() string { return s.user + "-session-" + s.session }

var selectorKeys = [...]string{"session", "ip", "ttl", "cidr"}

// parseSelector 拆出用户名里的后缀，顺序任意，除 -ip- 外可以组合：
//
//	<user>[-session-<id>][-ttl-<时长>][-cidr-<位数>]    或    <user>-ip-<addr>
//
// 第一个后缀之前是基础用户名；每个后缀的值到下一个后缀为止，所以会话 ID 里不能出现这些后缀。
// 时长是纯数字时按分钟计，也可以写 90s、10m、1h30m。出错时也会返回基础用户名，供认证使用。
func parseSelector(username string) (sel selector, err error) {
	type mark struct {
		at  int
		key string
	}
	var marks []mark
	for i := 0; i < len(username); i++ {
		for _, k := range selectorKeys {
			if strings.HasPrefix(username[i:], "-"+k+"-") {
				marks = append(marks, mark{i, k})
				i += len(k) + 1 // 跳过后缀名，紧跟在后面的 '-' 属于值
				break
			}
		}
	}
	sel.user = username
	if len(marks) > 0 {
		sel.user = username[:marks[0].at]
	}
	seen := map[string]bool{}
	for j, m := range marks {
		end := len(username)
		if j+1 < len(marks) {
			end = marks[j+1].at
		}
		v := username[m.at+len(m.key)+2 : end]
		if seen[m.key] {
			return sel, fmt.Errorf("后缀 -%s- 重复", m.key)
		}
		seen[m.key] = true
		switch m.key {
		case "session":
			sel.session, sel.sticky = v, true
		case "ip":
			sel.ip = v
		case "ttl":
			if n, err := strconv.Atoi(v); err == nil {
				v = strconv.Itoa(n) + "m"
			}
			if sel.ttl, err = time.ParseDuration(v); err != nil || sel.ttl <= 0 {
				return sel, fmt.Errorf("-ttl-%s 无效", v)
			}
			sel.sticky = true
		case "cidr":
			if sel.cidr, err = strconv.Atoi(v); err != nil || sel.cidr < 1 || sel.cidr > 128 {
				return sel, fmt.Errorf("-cidr-%s 无效，位数应为 1~128", v)
			}
		}
	}
	if seen["ip"] && len(marks) > 1 {
		return sel, errors.New("-ip- 不能和其他后缀一起使用")
	}
	return sel, nil
}

// relay 双向转发：一个方向读到 EOF（err == nil）时，只对另一端做 CloseWrite
// 半关闭，把 EOF 转达给对面协议栈，而不打断仍在传输的另一个方向（例如客户端
// 发完请求就半关闭，但还要收完响应）；支持 CloseWrite 的类型目前只有
// *net.TCPConn 和 gonet 的 *TCPConn，不支持时退化为整个关闭。一个方向出错时
// 直接关闭两端，解除另一个方向的阻塞。两个方向都结束后再关闭两端收尾。
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			} else {
				dst.Close()
			}
		} else {
			a.Close()
			b.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
