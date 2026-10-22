package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"
)

var (
	localPrefix  = netip.MustParsePrefix("2001:db8:1::/48")
	remotePrefix = netip.MustParsePrefix("2001:db8:ffff::/64")
	remoteAddr   = netip.MustParseAddr("2001:db8:ffff::1")
)

func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func ctxTimeout(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// pair 用两个 UDP socket 模拟 6in4 链路：local 是被测隧道，remote 扮演互联网上的主机。
// peerConn 是 local 眼中 peer（HE 服务器）的 socket。
func pair(t *testing.T) (local, remote *Tunnel, peerConn net.PacketConn) {
	t.Helper()
	a, b := listenUDP(t), listenUDP(t)
	var err error
	if local, err = NewWithConn(Config{Prefixes: []netip.Prefix{localPrefix}}, a, b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if remote, err = NewWithConn(Config{Prefixes: []netip.Prefix{remotePrefix}}, b, a.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close(); remote.Close() })
	return local, remote, b
}

func TestDialTCP(t *testing.T) {
	local, remote, _ := pair(t)
	ln, err := remote.ListenTCP(netip.AddrPortFrom(remoteAddr, 7))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, c.RemoteAddr().String())
			c.Close()
		}
	}()

	explicit := netip.MustParseAddr("2001:db8:1:2::3")
	for _, src := range []netip.Addr{explicit, {}} {
		c, err := local.Dial(ctxTimeout(t), src, "tcp", "[2001:db8:ffff::1]:7")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(c)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := netip.MustParseAddrPort(string(b)).Addr()
		if (src.IsValid() && got != src) || !localPrefix.Contains(got) {
			t.Fatalf("对端看到的源地址 %s（指定 %v，前缀 %s）", got, src, localPrefix)
		}
	}
}

func TestDialUDP(t *testing.T) {
	local, remote, _ := pair(t)
	srv, err := remote.ListenUDP(netip.AddrPortFrom(remoteAddr, 53))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := srv.ReadFrom(buf)
			if err != nil {
				return
			}
			srv.WriteTo([]byte(from.String()), from)
		}
	}()

	src := local.StickyAddr("udp")
	c, err := local.Dial(ctxTimeout(t), src, "udp", "[2001:db8:ffff::1]:53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := netip.MustParseAddrPort(string(buf[:n])).Addr(); got != src {
		t.Fatalf("对端看到的源地址 %s，期望 %s", got, src)
	}
}

func TestErrors(t *testing.T) {
	local, _, _ := pair(t)
	outside := netip.MustParseAddr("2001:db8:2::1")
	if _, err := local.Dial(ctxTimeout(t), outside, "tcp", "[2001:db8:ffff::1]:7"); !errors.Is(err, ErrNotInPool) {
		t.Fatalf("池外源地址: err = %v", err)
	}
	if _, err := local.ListenUDP(netip.AddrPortFrom(outside, 0)); !errors.Is(err, ErrNotInPool) {
		t.Fatalf("ListenUDP 池外地址: err = %v", err)
	}
	if _, err := local.Dial(ctxTimeout(t), netip.Addr{}, "tcp", "192.0.2.1:80"); !errors.Is(err, ErrNoIPv6) {
		t.Fatalf("IPv4 目标: err = %v", err)
	}
	if _, err := NewWithConn(Config{Prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}, listenUDP(t), nil); err == nil {
		t.Fatal("IPv4 前缀应当报错")
	}
	if _, err := NewWithConn(Config{Prefixes: []netip.Prefix{localPrefix}, MTU: 1000}, listenUDP(t), nil); err == nil {
		t.Fatal("MTU < 1280 应当报错")
	}
	unsupported := &net.UnixAddr{Name: "x", Net: "unixgram"}
	if _, err := NewWithConn(Config{Prefixes: []netip.Prefix{localPrefix}}, listenUDP(t), unsupported); err == nil {
		t.Fatal("peer 为不支持的地址类型应当报错")
	}
}

// Resolve 在域名查询失败时必须保留原始错误：既能用 errors.Is(err, ErrNoIPv6) 判断
// “没有 IPv6 地址”，也能用 errors.Is(err, context.Canceled) 之类判断具体原因。
func TestResolveDNSErrorCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Resolve(ctx, "example.invalid:80")
	if !errors.Is(err, ErrNoIPv6) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望同时包裹 ErrNoIPv6 与 context.Canceled", err)
	}
}

// 内核 NIC 开启 GRO 时会把连续的内层 TCP/UDP 分段合并成超过隧道 MTU 的超级包，
// 合并前内核已逐段校验过校验和，但合并后不会重算内层校验和字段（可能残留某一
// 分段的旧值）。不超过 MTU 的包必须按常规规则校验；超过 MTU 的包只能是 GRO
// 产物，应当放行，即使校验和字段本身是错的。
func TestOversizedPacketSkipsChecksum(t *testing.T) {
	local, _, peerConn := pair(t)
	ln, err := local.ListenUDP(netip.MustParseAddrPort("[2001:db8:1::1]:9"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	src, dst := netip.MustParseAddr("2001:db8:ffff::9"), netip.MustParseAddr("2001:db8:1::1")
	srcPort, dstPort := uint16(5353), uint16(9)
	wrongChecksum := uint16(0x1234) // 故意写错且非零，模拟 GRO 合并后残留的旧校验和
	put16 := func(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }

	// 手工拼一个 IPv6 + UDP 包：pkt[0] 是 version，pkt[4:6] 是 payload length，
	// pkt[6]/pkt[7] 是 next header(UDP=17)/hop limit，接着是 16 字节源地址、
	// 16 字节目的地址，再是 UDP 头（src port/dst port/length/checksum）和数据。
	packet := func(data []byte) []byte {
		pkt := make([]byte, 40+8+len(data))
		udpLen := uint16(8 + len(data))
		pkt[0] = 0x60
		put16(pkt[4:6], udpLen)
		pkt[6], pkt[7] = 17, 64
		s, d := src.As16(), dst.As16()
		copy(pkt[8:24], s[:])
		copy(pkt[24:40], d[:])
		put16(pkt[40:42], srcPort)
		put16(pkt[42:44], dstPort)
		put16(pkt[44:46], udpLen)
		put16(pkt[46:48], wrongChecksum)
		copy(pkt[48:], data)
		return pkt
	}
	fill := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}

	buf := make([]byte, 2200)
	checksumErrors := local.stack.Stats().UDP.ChecksumErrors

	// 小包（未超过 MTU）：错误的校验和必须按常规规则被拒收，不能收到数据。
	small := fill(100) // 40 + 8 + 100 = 148 字节，远小于 MTU 1480
	before := checksumErrors.Value()
	peerConn.WriteTo(packet(small), local.pc.LocalAddr())
	ln.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := ln.ReadFrom(buf); err == nil {
		t.Fatal("校验和错误的小包（未超过 MTU）竟然被当作正常数据收到了")
	}
	if checksumErrors.Value() == before {
		t.Fatal("UDP.ChecksumErrors 应该增加")
	}

	// 大包（超过 MTU）：只能是 GRO 合并产物，必须放行，哪怕校验和字段是错的。
	big := fill(2000) // 40 + 8 + 2000 = 2048 字节，超过 MTU 1480
	peerConn.WriteTo(packet(big), local.pc.LocalAddr())
	ln.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ln.ReadFrom(buf)
	if err != nil {
		t.Fatalf("超过 MTU 的包应被视为 GRO 产物放行: %v", err)
	}
	if !bytes.Equal(buf[:n], big) {
		t.Fatal("收到的 payload 与发出的数据不一致")
	}
}

// 只有来自 peer、且目的地址在池内的 6in4 包才会被注入协议栈。
func TestDropForeign(t *testing.T) {
	local, _, peerConn := pair(t)
	attacker := listenUDP(t)
	packet := func(dst string) []byte {
		pkt := make([]byte, 40) // 最小 IPv6 包：Next Header = 59（No Next Header），无负载
		pkt[0], pkt[6], pkt[7] = 0x60, 59, 64
		s, d := netip.MustParseAddr("2001:db8:ffff::9").As16(), netip.MustParseAddr(dst).As16()
		copy(pkt[8:24], s[:])
		copy(pkt[24:40], d[:])
		return pkt
	}
	received := local.stack.Stats().IP.PacketsReceived
	before := received.Value()

	attacker.WriteTo(packet("2001:db8:1::1"), local.pc.LocalAddr())
	peerConn.WriteTo(packet("2001:db8:2::1"), local.pc.LocalAddr())
	time.Sleep(200 * time.Millisecond)
	if received.Value() != before {
		t.Fatal("接受了非 peer 发来的包，或目的地址不在池内的包")
	}

	peerConn.WriteTo(packet("2001:db8:1::1"), local.pc.LocalAddr())
	deadline := time.Now().Add(2 * time.Second)
	for received.Value() == before {
		if time.Now().After(deadline) {
			t.Fatal("没有接受 peer 发来的合法包")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
