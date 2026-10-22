// Package tunnel 在用户态接入 HE 6in4 隧道：raw socket 收发 IPv4 协议 41，
// gVisor netstack 处理 IPv6，不创建任何网卡，并且可以用池内任意地址作为源地址。
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const nicID = 1

var (
	ErrNotInPool = errors.New("tunnel: 源地址不在地址池内")
	ErrNoIPv6    = errors.New("tunnel: 目标没有 IPv6 地址")
)

// Config 对应 tunnelbroker.net 隧道详情页里的信息。
type Config struct {
	ServerIPv4 netip.Addr     // HE "Server IPv4 Address"
	LocalIPv4  netip.Addr     // 可选，本地绑定地址；NAT 后填内网 IP，零值为 0.0.0.0
	Prefixes   []netip.Prefix // 可用作源地址的 IPv6 前缀（Routed /64、/48 等）
	MTU        int            // 隧道 MTU，0 表示 1480
}

// Tunnel 是一条用户态 6in4 隧道。
type Tunnel struct {
	pool   pool
	stack  *stack.Stack
	ep     *channel.Endpoint
	rxCap  *rxCapEndpoint
	pc     net.PacketConn
	peer   net.Addr
	peerAP netip.AddrPort
	mtu    int
	cancel context.CancelFunc
	wg     sync.WaitGroup

	spreadSeq atomic.Uint64 // SpreadAddr 的序号，从随机值开始，重启后不会重放同样的子网顺序
}

// rxCapEndpoint 包装 channel.Endpoint，只覆盖 Capabilities()：gVisor 的
// nic.DeliverNetworkPacket 在把包交给协议栈之前，会用链路层的
// CapabilityRXChecksumOffload 能力位无条件覆盖 PacketBuffer.RXChecksumValidated
// （见 pkg/tcpip/stack/nic.go），所以直接给某个包的 PacketBuffer 赋值不会生效——
// 必须通过能力位才能让"这个包免校验"的决定真正传达到协议栈。
// Capabilities() 在 DeliverNetworkPacket 里是每次同步、实时读取的，而 inbound()
// 是唯一调用 InjectInbound 的协程（单协程、同步），所以在注入前设置 skipRXChecksum
// 不存在并发问题；它和用于发送方向的 CapabilityTXChecksumOffload 是独立的位，
// 不影响出方向的校验和计算。
type rxCapEndpoint struct {
	*channel.Endpoint
	skipRXChecksum atomic.Bool
}

func (e *rxCapEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	caps := e.Endpoint.Capabilities()
	if e.skipRXChecksum.Load() {
		caps |= stack.CapabilityRXChecksumOffload
	}
	return caps
}

// New 创建 raw socket（需要 root 或 CAP_NET_RAW）并启动隧道。
func New(cfg Config) (*Tunnel, error) {
	if !cfg.ServerIPv4.Is4() {
		return nil, errors.New("tunnel: ServerIPv4 必须是 IPv4 地址")
	}
	local := "0.0.0.0"
	if cfg.LocalIPv4.IsValid() {
		if !cfg.LocalIPv4.Is4() {
			return nil, errors.New("tunnel: LocalIPv4 必须是 IPv4 地址")
		}
		local = cfg.LocalIPv4.String()
	}
	pc, err := net.ListenPacket("ip4:41", local)
	if err != nil {
		return nil, fmt.Errorf("tunnel: 创建 raw socket 失败（需要 root 或 CAP_NET_RAW）: %w", err)
	}
	t, err := NewWithConn(cfg, pc, &net.IPAddr{IP: cfg.ServerIPv4.AsSlice()})
	if err != nil {
		pc.Close()
		return nil, err
	}
	return t, nil
}

// NewWithConn 在任意 PacketConn 上运行隧道：pc 上收发的每个数据报都是一个裸 IPv6 包，
// 只接受来自 peer 的包，发出的包都写给 peer。New 用它跑在 raw socket 上，测试用它跑在 UDP 上。
// 成功后 pc 归 Tunnel 所有，由 Close 关闭。
func NewWithConn(cfg Config, pc net.PacketConn, peer net.Addr) (*Tunnel, error) {
	if len(cfg.Prefixes) == 0 {
		return nil, errors.New("tunnel: 至少需要一个 IPv6 前缀")
	}
	p := make(pool, len(cfg.Prefixes))
	for i, pfx := range cfg.Prefixes {
		if !pfx.IsValid() || !pfx.Addr().Is6() || pfx.Addr().Is4In6() {
			return nil, fmt.Errorf("tunnel: %s 不是 IPv6 前缀", pfx)
		}
		p[i] = pfx.Masked()
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1480
	}
	if mtu < header.IPv6MinimumMTU || mtu > 65535 {
		return nil, fmt.Errorf("tunnel: MTU %d 超出范围 [1280, 65535]", mtu)
	}
	// addrPort 对不认识的 net.Addr 类型（包括 nil）返回零值 AddrPort；如果不在这里
	// 挡住，inbound() 里 "来源==peer" 的判断会退化成零值与零值相等，等于对任何
	// 发送者都放行，彻底失去来源过滤。
	if !addrPort(peer).Addr().IsValid() {
		return nil, errors.New("tunnel: peer 必须是 *net.IPAddr 或 *net.UDPAddr")
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol6},
	})
	ep := channel.New(1024, uint32(mtu), "")
	rxEP := &rxCapEndpoint{Endpoint: ep}
	if err := s.CreateNIC(nicID, rxEP); err != nil {
		s.Destroy()
		return nil, fmt.Errorf("tunnel: CreateNIC: %s", err)
	}
	// Spoofing：允许 bind 和发送任意源地址；Promiscuous：接收发往任意地址的包。
	s.SetSpoofing(nicID, true)
	s.SetPromiscuousMode(nicID, true)
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv6EmptySubnet, NIC: nicID}})
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	ctx, cancel := context.WithCancel(context.Background())
	t := &Tunnel{pool: p, stack: s, ep: ep, rxCap: rxEP, pc: pc, peer: peer, peerAP: addrPort(peer), mtu: mtu, cancel: cancel}
	t.spreadSeq.Store(rand.Uint64())
	t.wg.Add(2)
	go t.inbound()
	go t.outbound(ctx)
	return t, nil
}

// inbound 把 peer 发来的 IPv6 包注入 netstack。
func (t *Tunnel) inbound() {
	defer t.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := t.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Debug("tunnel: 读取失败", "err", err)
			continue
		}
		if addrPort(from) != t.peerAP {
			continue // 只接受 HE 服务器发来的包，防止伪造注入
		}
		pkt := buf[:n]
		if len(pkt) < header.IPv6MinimumSize || header.IPVersion(pkt) != header.IPv6Version {
			continue
		}
		ip := header.IPv6(pkt)
		end := header.IPv6MinimumSize + int(ip.PayloadLength())
		if end > len(pkt) {
			continue // 截断的包
		}
		if !t.pool.contains(netip.AddrFrom16(ip.DestinationAddress().As16())) {
			continue
		}
		pb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(pkt[:end])})
		// 超过隧道 MTU 的包通常是内核 GRO 合并出的超级包：内核在合并前已对每个
		// 分段逐一校验过 TCP/UDP 校验和，合并后不会重算内层校验和（字段仍是某一
		// 分段的旧值），netstack 若按常规流程重新校验会误判为校验和错误而丢弃，
		// 所以要放行。通过 rxCap 临时上报 CapabilityRXChecksumOffload（见其注释），
		// 让 nic.DeliverNetworkPacket 把这个决定写进 PacketBuffer.RXChecksumValidated。
		// ponytail: GRO 合并出的包若 ≤ MTU（发送方 MSS ≲ 720）仍会因旧校验和被丢；
		// MTU 配得比 HE 小时，IPv4 分片重组出的大包会跳过校验；这个判断依赖 gvisor
		// nic.go 用链路能力覆盖 RXChecksumValidated 的现状，升级 gVisor 时由
		// TestOversizedPacketSkipsChecksum 兜底。
		t.rxCap.skipRXChecksum.Store(end > t.mtu)
		t.ep.InjectInbound(ipv6.ProtocolNumber, pb)
		pb.DecRef()
	}
}

// outbound 把 netstack 发出的 IPv6 包交给 peer（raw socket 会由内核补上 IPv4 头）。
func (t *Tunnel) outbound(ctx context.Context) {
	defer t.wg.Done()
	for {
		pkt := t.ep.ReadContext(ctx)
		if pkt == nil {
			return // ctx 已取消
		}
		v := pkt.ToView()
		_, err := t.pc.WriteTo(v.AsSlice(), t.peer)
		v.Release()
		pkt.DecRef()
		if err != nil {
			slog.Debug("tunnel: 发送失败", "err", err)
		}
	}
}

// addrPort 统一 raw socket（*net.IPAddr）和 UDP（*net.UDPAddr）的地址，便于比较来源。
func addrPort(a net.Addr) netip.AddrPort {
	switch a := a.(type) {
	case *net.IPAddr:
		ip, _ := netip.AddrFromSlice(a.IP)
		return netip.AddrPortFrom(ip.Unmap(), 0)
	case *net.UDPAddr:
		ap := a.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return netip.AddrPort{}
}

func fullAddr(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom16(ap.Addr().As16()), Port: ap.Port()}
}

// Resolve 把 host:port 解析成 IPv6 地址：域名只查询 AAAA，IPv4 目标返回 ErrNoIPv6。
func Resolve(ctx context.Context, address string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("tunnel: 端口无效 %q", port)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip6", host)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("%w: %s: %w", ErrNoIPv6, host, err)
		}
		if len(ips) == 0 {
			return netip.AddrPort{}, fmt.Errorf("%w: %s", ErrNoIPv6, host)
		}
		ip = ips[0]
	}
	if !ip.Is6() || ip.Is4In6() {
		return netip.AddrPort{}, fmt.Errorf("%w: %s", ErrNoIPv6, host)
	}
	return netip.AddrPortFrom(ip.WithZone(""), uint16(p)), nil
}

// Dial 以 src 为源地址连接 address（host:port），network 为 tcp/tcp6/udp/udp6。
// src 为零值时随机选择池内地址。
func (t *Tunnel) Dial(ctx context.Context, src netip.Addr, network, address string) (net.Conn, error) {
	if !src.IsValid() {
		src = t.pool.random()
	} else if !t.pool.contains(src) {
		return nil, ErrNotInPool
	}
	dst, err := Resolve(ctx, address)
	if err != nil {
		return nil, err
	}
	local, remote := fullAddr(netip.AddrPortFrom(src, 0)), fullAddr(dst)
	switch network {
	case "tcp", "tcp6":
		c, err := gonet.DialTCPWithBind(ctx, t.stack, local, remote, ipv6.ProtocolNumber)
		if err != nil {
			return nil, err // 不能直接返回 c：nil 的 *TCPConn 装进接口后不等于 nil
		}
		return c, nil
	case "udp", "udp6":
		c, err := gonet.DialUDP(t.stack, &local, &remote, ipv6.ProtocolNumber)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, fmt.Errorf("tunnel: 不支持的 network %q", network)
}

// ListenUDP 在 src 上打开一个非 connected 的 UDP socket，端口为 0 时自动分配。
// 返回的 conn 是 gonet 的 UDP 实现，WriteTo 只接受 *net.UDPAddr。
func (t *Tunnel) ListenUDP(src netip.AddrPort) (net.PacketConn, error) {
	if !t.pool.contains(src.Addr()) {
		return nil, ErrNotInPool
	}
	local := fullAddr(src)
	c, err := gonet.DialUDP(t.stack, &local, nil, ipv6.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListenTCP 在池内地址上监听 TCP，用于接受入站连接。
func (t *Tunnel) ListenTCP(addr netip.AddrPort) (net.Listener, error) {
	if !t.pool.contains(addr.Addr()) {
		return nil, ErrNotInPool
	}
	l, err := gonet.ListenTCP(t.stack, fullAddr(addr), ipv6.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// RandomAddr 返回池内随机地址。
func (t *Tunnel) RandomAddr() netip.Addr { return t.pool.random() }

// SpreadAddr 返回池内地址，按 /cidr 子网轮换：每次调用落在不同的子网，用遍所有子网之后才会重复。
func (t *Tunnel) SpreadAddr(cidr int) netip.Addr { return t.pool.spread(cidr, t.spreadSeq.Add(1)) }

// StickyAddr 返回 key 对应的固定地址。
func (t *Tunnel) StickyAddr(key string) netip.Addr { return t.pool.sticky(key) }

// Contains 判断 a 是否在池内。
func (t *Tunnel) Contains(a netip.Addr) bool { return t.pool.contains(a) }

// Close 关闭隧道以及隧道上的所有连接。
func (t *Tunnel) Close() error {
	t.cancel()
	err := t.pc.Close()
	t.stack.Destroy()
	t.wg.Wait()
	return err
}
