package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

// SOCKS5 常量（RFC 1928）。
const (
	socksVer     = 5
	cmdConnect   = 1
	cmdAssociate = 3
	atypIPv4     = 1
	atypDomain   = 3
	atypIPv6     = 4

	repOK              = 0
	repFail            = 1
	repNotAllowed      = 2
	repHostUnreach     = 4
	repRefused         = 5
	repCmdUnsupported  = 7
	repAtypUnsupported = 8
)

func (p *Proxy) socks5(c net.Conn) error {
	c.SetDeadline(time.Now().Add(dialTimeout)) // 握手超时，进入转发前清除
	src, srcErr := p.socksAuth(c)
	if srcErr != nil && !errors.Is(srcErr, tunnel.ErrNotInPool) {
		return srcErr
	}
	// 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return err
	}
	dst, err := readAddr(c)
	if err != nil {
		writeReply(c, repAtypUnsupported, netip.AddrPort{})
		return err
	}
	if srcErr != nil {
		writeReply(c, repNotAllowed, netip.AddrPort{})
		return srcErr
	}
	switch h[1] {
	case cmdConnect:
		return p.socksConnect(c, src, dst)
	case cmdAssociate:
		return p.socksAssociate(c, src)
	}
	writeReply(c, repCmdUnsupported, netip.AddrPort{})
	return fmt.Errorf("不支持的命令 %d", h[1])
}

// socksAuth 完成方法协商和认证（RFC 1928 §3、RFC 1929），返回源地址。
// 返回 tunnel.ErrNotInPool 表示认证通过但显式 IP 无效，留到请求阶段回复 0x02。
func (p *Proxy) socksAuth(c net.Conn) (netip.Addr, error) {
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return netip.Addr{}, err
	}
	if h[0] != socksVer {
		return netip.Addr{}, fmt.Errorf("不支持的 SOCKS 版本 %d", h[0])
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return netip.Addr{}, err
	}
	switch {
	case bytes.IndexByte(methods, 2) >= 0:
		// 用户名密码认证；没有配置 users 时也优先用它，以便拿到用户名里的选择器
		if _, err := c.Write([]byte{socksVer, 2}); err != nil {
			return netip.Addr{}, err
		}
		user, pass, err := readUserPass(c)
		if err != nil {
			return netip.Addr{}, err
		}
		src, err := p.source(user, pass)
		if errors.Is(err, errAuth) {
			c.Write([]byte{1, 1})
			return netip.Addr{}, err
		}
		if _, werr := c.Write([]byte{1, 0}); werr != nil {
			return netip.Addr{}, werr
		}
		return src, err
	case len(p.Users) == 0 && bytes.IndexByte(methods, 0) >= 0:
		_, err := c.Write([]byte{socksVer, 0})
		return netip.Addr{}, err
	}
	c.Write([]byte{socksVer, 0xff})
	return netip.Addr{}, errors.New("没有可接受的认证方法")
}

// readUserPass 读取 RFC 1929 的 VER ULEN UNAME PLEN PASSWD。
func readUserPass(r io.Reader) (user, pass string, err error) {
	var b [2]byte
	if _, err = io.ReadFull(r, b[:]); err != nil {
		return
	}
	u := make([]byte, b[1])
	if _, err = io.ReadFull(r, u); err != nil {
		return
	}
	if _, err = io.ReadFull(r, b[:1]); err != nil {
		return
	}
	pw := make([]byte, b[0])
	if _, err = io.ReadFull(r, pw); err != nil {
		return
	}
	return string(u), string(pw), nil
}

// readAddr 读取 ATYP DST.ADDR DST.PORT，返回 host:port。
func readAddr(r io.Reader) (string, error) {
	var atyp [1]byte
	if _, err := io.ReadFull(r, atyp[:]); err != nil {
		return "", err
	}
	var host string
	switch atyp[0] {
	case atypIPv4, atypIPv6:
		b := make([]byte, 4)
		if atyp[0] == atypIPv6 {
			b = make([]byte, 16)
		}
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		a, _ := netip.AddrFromSlice(b)
		host = a.String()
	case atypDomain:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return "", err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		return "", fmt.Errorf("不支持的地址类型 %d", atyp[0])
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// appendAddr 以 SOCKS5 格式（ATYP ADDR PORT）追加地址；零值写成 IPv4 0.0.0.0:0。
func appendAddr(b []byte, ap netip.AddrPort) []byte {
	a := ap.Addr().Unmap()
	switch {
	case a.Is4():
		b = append(b, atypIPv4)
		b = append(b, a.AsSlice()...)
	case a.Is6():
		b = append(b, atypIPv6)
		b = append(b, a.AsSlice()...)
	default:
		b = append(b, atypIPv4, 0, 0, 0, 0)
	}
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

func writeReply(w io.Writer, rep byte, bind netip.AddrPort) error {
	_, err := w.Write(appendAddr([]byte{socksVer, rep, 0}, bind))
	return err
}

// dialReply 把拨号错误映射成 SOCKS5 应答码。
func dialReply(err error) byte {
	switch {
	case errors.Is(err, tunnel.ErrNotInPool):
		return repNotAllowed
	case errors.Is(err, tunnel.ErrNoIPv6):
		return repHostUnreach
	case strings.Contains(err.Error(), "refused"): // gonet 把 tcpip 错误转成了纯文本，只能按文本匹配
		return repRefused
	}
	return repFail
}

func (p *Proxy) socksConnect(c net.Conn, src netip.Addr, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	up, err := p.Tunnel.Dial(ctx, src, "tcp", dst)
	if err != nil {
		writeReply(c, dialReply(err), netip.AddrPort{})
		return err
	}
	defer up.Close()
	bind, _ := netip.ParseAddrPort(up.LocalAddr().String())
	if err := writeReply(c, repOK, bind); err != nil {
		return err
	}
	c.SetDeadline(time.Time{})
	relay(c, up)
	return nil
}

// socksAssociate 处理 UDP ASSOCIATE（RFC 1928 §7）：本地 UDP socket 与客户端通信，
// 隧道侧 UDP socket 以 src 为源地址与目标通信，控制连接断开时一并关闭。
func (p *Proxy) socksAssociate(c net.Conn, src netip.Addr) error {
	if !src.IsValid() {
		src = p.Tunnel.RandomAddr()
	}
	out, err := p.Tunnel.ListenUDP(netip.AddrPortFrom(src, 0))
	if err != nil {
		writeReply(c, dialReply(err), netip.AddrPort{})
		return err
	}
	defer out.Close()
	// 绑定在控制连接的本地地址上，这样监听 0.0.0.0 时也能回复一个有效地址
	in, err := net.ListenUDP("udp", &net.UDPAddr{IP: c.LocalAddr().(*net.TCPAddr).IP})
	if err != nil {
		writeReply(c, repFail, netip.AddrPort{})
		return err
	}
	defer in.Close()
	if err := writeReply(c, repOK, in.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		return err
	}
	c.SetDeadline(time.Time{})

	clientIP := c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	var client atomic.Pointer[net.UDPAddr] // 客户端最近一次发包的地址

	go func() { // 目标 → 客户端
		buf := make([]byte, 65535)
		for {
			n, from, err := out.ReadFrom(buf)
			if err != nil {
				return
			}
			if to := client.Load(); to != nil {
				pkt := appendAddr([]byte{0, 0, 0}, from.(*net.UDPAddr).AddrPort())
				in.WriteToUDP(append(pkt, buf[:n]...), to)
			}
		}
	}()
	go func() { // 客户端 → 目标
		buf := make([]byte, 65535)
		for {
			n, from, err := in.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if from.AddrPort().Addr().Unmap() != clientIP || n < 4 || buf[2] != 0 {
				continue // 只接受控制连接所属的客户端；不支持分片
			}
			r := bytes.NewReader(buf[3:n])
			dst, err := readAddr(r)
			if err != nil {
				continue
			}
			// ponytail: 域名目标每个包都解析一次，需要时再加缓存
			ap, err := tunnel.Resolve(context.Background(), dst)
			if err != nil {
				continue
			}
			client.Store(from)
			out.WriteTo(buf[n-r.Len():n], net.UDPAddrFromAddrPort(ap))
		}
	}()
	io.Copy(io.Discard, c) // 阻塞到控制连接断开
	return nil
}
