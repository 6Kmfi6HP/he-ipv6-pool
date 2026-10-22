package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/he-ipv6-pool/tunnel"
)

var remoteAddr = netip.MustParseAddr("2001:db8:ffff::1")

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func listenUDP(t *testing.T) net.PacketConn {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// setup 搭建测试拓扑：被测隧道 local（池 2001:db8:1::/48）通过 UDP 连到扮演互联网的 remote 隧道。
// remote 上运行：80 端口 HTTP（响应体为请求方 IP）、7 端口 TCP echo、7 端口 UDP（回复请求方地址）、
// 8 端口 TCP（读到 EOF 后回写 "resp:" + 收到的内容，用于验证半关闭）。
func setup(t *testing.T, users map[string]string) (*tunnel.Tunnel, *Proxy) {
	t.Helper()
	a, b := listenUDP(t), listenUDP(t)
	local, err := tunnel.NewWithConn(tunnel.Config{Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:1::/48")}}, a, b.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := tunnel.NewWithConn(tunnel.Config{Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8:ffff::/64")}}, b, a.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close(); remote.Close() })

	web, err := remote.ListenTCP(netip.AddrPortFrom(remoteAddr, 80))
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(web, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, netip.MustParseAddrPort(r.RemoteAddr).Addr().String())
	}))

	echo, err := remote.ListenTCP(netip.AddrPortFrom(remoteAddr, 7))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	halfClose, err := remote.ListenTCP(netip.AddrPortFrom(remoteAddr, 8))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := halfClose.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				b, _ := io.ReadAll(c)
				io.WriteString(c, "resp:"+string(b))
				c.Close()
			}(c)
		}
	}()

	udp, err := remote.ListenUDP(netip.AddrPortFrom(remoteAddr, 7))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			_, from, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			udp.WriteTo([]byte(from.String()), from)
		}
	}()
	return local, &Proxy{Tunnel: local, Users: users}
}

// serveProxy 在随机端口上运行 p.Serve（SOCKS5 和 HTTP 共用），返回监听地址。
func serveProxy(t *testing.T, p *Proxy) string {
	l := listenTCP(t)
	go p.Serve(l)
	return l.Addr().String()
}

// get 通过代理 proxyURL 请求 remote 上的 HTTP 服务，返回状态码和响应体（remote 看到的源 IP）。
// 连接失败时状态码为 0，响应体为错误信息。
func get(t *testing.T, proxyURL string) (int, string) {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 5 * time.Second}
	resp, err := c.Get("http://[2001:db8:ffff::1]/")
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestParseSelector(t *testing.T) {
	cases := []struct {
		in   string
		want selector
	}{
		{"alice", selector{user: "alice"}},
		{"", selector{}},
		{"alice-session-abc", selector{user: "alice", session: "abc", sticky: true}},
		{"alice-ip-2001-db8-1--42", selector{user: "alice", ip: "2001-db8-1--42"}},
		{"alice-cidr-64", selector{user: "alice", cidr: 64}},
		{"alice-ttl-10", selector{user: "alice", ttl: 10 * time.Minute, sticky: true}},
		// 组合使用，顺序任意；会话 ID 可以带 -；用户名里的 - 不受影响
		{"my-name-session-a-b-ttl-10-cidr-64", selector{user: "my-name", session: "a-b", ttl: 10 * time.Minute, cidr: 64, sticky: true}},
		{"alice-cidr-56-ttl-1h30m-session-x", selector{user: "alice", session: "x", ttl: 90 * time.Minute, cidr: 56, sticky: true}},
		{"alice-session-ip-x", selector{user: "alice", session: "ip-x", sticky: true}}, // 紧跟在后缀名后面的不算新后缀
	}
	for _, c := range cases {
		if got, err := parseSelector(c.in); err != nil || got != c.want {
			t.Errorf("parseSelector(%q) = %+v, %v；期望 %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{
		"alice-ip-2001-db8-1--1-session-x", // -ip- 不能组合
		"alice-session-a-session-b",        // 重复
		"alice-cidr-0", "alice-cidr-129", "alice-cidr-x",
		"alice-ttl-0", "alice-ttl-x", "alice-ttl--5",
	} {
		sel, err := parseSelector(in)
		if err == nil || sel.user != "alice" {
			t.Errorf("parseSelector(%q) 应当报错并且仍然拆出用户名: %+v, %v", in, sel, err)
		}
	}
}

// 会话、生存时间、cidr 组合：同一个会话在生存时间内固定 IP，到期后换新；带 cidr 时不同会话落在不同子网。
func TestSessionTTL(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	addr := serveProxy(t, p)
	ip := func(user string) string {
		t.Helper()
		code, body := get(t, "socks5://"+user+":pw@"+addr)
		if code != 200 || !local.Contains(netip.MustParseAddr(body)) {
			t.Fatalf("%s: %d %s", user, code, body)
		}
		return body
	}

	// 到期换新由 TestSessions 覆盖：-race 下每个请求要几百毫秒，这里没法卡准时间
	if a, b := ip("alice-session-t-ttl-10"), ip("alice-session-t-ttl-10"); a != b {
		t.Fatalf("生存时间内应当固定: %s %s", a, b)
	}
	if a, b := ip("alice-ttl-10"), ip("alice-ttl-10"); a != b {
		t.Fatalf("只带 ttl 时整个用户共用一个 IP: %s %s", a, b)
	}

	seen := map[netip.Prefix]bool{}
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		u := "alice-session-" + id + "-cidr-64"
		a, b := ip(u), ip(u)
		s := netip.PrefixFrom(netip.MustParseAddr(a), 64).Masked()
		if a != b || seen[s] {
			t.Fatalf("%s: %s %s，子网重复 %v", u, a, b, seen[s])
		}
		seen[s] = true
	}

	for _, bad := range []string{"alice-ip-2001-db8-1--1-session-x", "alice-ttl-0"} {
		if code, body := get(t, "http://"+bad+":pw@"+addr); code != http.StatusForbidden {
			t.Errorf("%s 应当 403: %d %s", bad, code, body)
		}
	}
}

// 生存时间从第一次使用开始计时、使用不会延长，到期后换新地址；过期的会话在表增长时被清理。
func TestSessions(t *testing.T) {
	n := 0
	pick := func() netip.Addr { n++; return netip.AddrFrom16([16]byte{0: 0x20, 15: byte(n)}) }
	var s sessions
	a := s.get("k", 200*time.Millisecond, pick)
	time.Sleep(50 * time.Millisecond)
	if b := s.get("k", 200*time.Millisecond, pick); b != a {
		t.Fatalf("生存时间内应当复用: %s %s", a, b)
	}
	time.Sleep(200 * time.Millisecond)
	if b := s.get("k", 200*time.Millisecond, pick); b == a {
		t.Fatalf("到期后应当换新地址: %s", b)
	}

	var big sessions
	for i := range 1024 {
		big.get(strconv.Itoa(i), time.Nanosecond, pick)
	}
	time.Sleep(time.Millisecond)
	big.get("new", time.Minute, pick)
	if len(big.m) != 1 {
		t.Fatalf("过期会话没有被清理: 还剩 %d 个", len(big.m))
	}
}

// SOCKS5 和 HTTP 共用一个端口：按首字节分流，两种客户端都能用；
// 不是 0x05 开头的数据都交给 HTTP 服务器处理（垃圾数据得到 400）。
func TestMixedPort(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	addr := serveProxy(t, p)
	for _, scheme := range []string{"socks5", "http"} {
		if code, body := get(t, scheme+"://alice:pw@"+addr); code != 200 || !local.Contains(netip.MustParseAddr(body)) {
			t.Fatalf("%s: %d %s", scheme, code, body)
		}
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "\x01garbage\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(line, "HTTP/1.1 400") {
		t.Fatalf("垃圾数据应当由 HTTP 服务器回 400，得到 %q", line)
	}
}

// -cidr-<位数>：随机出口按 /位数 轮换子网，连续的连接不落在同一个子网；位数非法时拒绝。
func TestCIDR(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	addr := serveProxy(t, p)
	seen := map[netip.Prefix]bool{}
	for _, scheme := range []string{"socks5", "http", "socks5", "http"} {
		code, body := get(t, scheme+"://alice-cidr-64:pw@"+addr)
		if code != 200 || !local.Contains(netip.MustParseAddr(body)) {
			t.Fatalf("%s: %d %s", scheme, code, body)
		}
		s := netip.PrefixFrom(netip.MustParseAddr(body), 64).Masked()
		if seen[s] {
			t.Fatalf("%s 重复使用了子网 %s", body, s)
		}
		seen[s] = true
	}
	for _, bad := range []string{"0", "129", "x"} {
		if code, body := get(t, "http://alice-cidr-"+bad+":pw@"+addr); code != http.StatusForbidden {
			t.Errorf("cidr-%s 应当 403: %d %s", bad, code, body)
		}
		if code, _ := get(t, "socks5://alice-cidr-"+bad+":pw@"+addr); code != 0 {
			t.Errorf("socks5 cidr-%s 应当失败: %d", bad, code)
		}
	}
}

func TestSOCKS5(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	addr := serveProxy(t, p)

	if code, body := get(t, "socks5://alice:pw@"+addr); code != 200 || !local.Contains(netip.MustParseAddr(body)) {
		t.Fatalf("随机: %d %s", code, body)
	}
	want := local.StickyAddr("alice-session-s1").String()
	for range 2 {
		if code, body := get(t, "socks5://alice-session-s1:pw@"+addr); code != 200 || body != want {
			t.Fatalf("session: %d %s，期望 %s", code, body, want)
		}
	}
	if code, body := get(t, "socks5://alice-ip-2001-db8-1--42:pw@"+addr); code != 200 || body != "2001:db8:1::42" {
		t.Fatalf("显式 IP: %d %s", code, body)
	}
	if code, body := get(t, "socks5://alice-ip-2001-db8-2--1:pw@"+addr); code != 0 {
		t.Fatalf("池外显式 IP 应当失败: %d %s", code, body)
	}
	if code, body := get(t, "socks5://alice:wrong@"+addr); code != 0 {
		t.Fatalf("错误密码应当失败: %d %s", code, body)
	}
}

func TestSOCKS5NoAuth(t *testing.T) {
	local, p := setup(t, nil)
	addr := serveProxy(t, p)
	if code, body := get(t, "socks5://"+addr); code != 200 || !local.Contains(netip.MustParseAddr(body)) {
		t.Fatalf("无认证: %d %s", code, body)
	}
	if code, body := get(t, "socks5://x-session-a:any@"+addr); code != 200 || body != local.StickyAddr("x-session-a").String() {
		t.Fatalf("无认证 + session: %d %s", code, body)
	}
}

func TestSOCKS5UDP(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	c, err := net.Dial("tcp", serveProxy(t, p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	user := "alice-session-udp"
	c.Write([]byte{5, 1, 2})
	c.Write(append(append([]byte{1, byte(len(user))}, user...), 2, 'p', 'w'))
	c.Write([]byte{5, cmdAssociate, 0, atypIPv4, 0, 0, 0, 0, 0, 0})
	resp := make([]byte, 2+2+10) // 方法选择 + 认证结果 + 应答（BND 为 IPv4）
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[1] != 2 || resp[3] != 0 || resp[5] != repOK || resp[7] != atypIPv4 {
		t.Fatalf("握手应答异常: %v", resp)
	}
	bnd := netip.AddrPortFrom(netip.AddrFrom4([4]byte(resp[8:12])), binary.BigEndian.Uint16(resp[12:14]))

	u, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(bnd))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(5 * time.Second))
	hdr := appendAddr([]byte{0, 0, 0}, netip.AddrPortFrom(remoteAddr, 7))
	if _, err := u.Write(append(hdr, "ping"...)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n < len(hdr) || !bytes.Equal(buf[:len(hdr)], hdr) {
		t.Fatalf("UDP 应答头异常: %v", buf[:n])
	}
	got := netip.MustParseAddrPort(string(buf[len(hdr):n])).Addr()
	if want := local.StickyAddr(user); got != want {
		t.Fatalf("对端看到的源地址 %s，期望 %s", got, want)
	}
}

// 客户端发完请求后 CloseWrite（半关闭）必须仍能收到完整响应：relay 不能在某一
// 方向读到 EOF 时就把两端整个关掉，而要把 EOF 转换成对另一端的 CloseWrite。
func TestSOCKS5HalfClose(t *testing.T) {
	_, p := setup(t, map[string]string{"alice": "pw"})
	c, err := net.Dial("tcp", serveProxy(t, p))
	if err != nil {
		t.Fatal(err)
	}
	tc := c.(*net.TCPConn)
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(5 * time.Second))

	user := "alice"
	tc.Write([]byte{5, 1, 2})
	tc.Write(append(append([]byte{1, byte(len(user))}, user...), 2, 'p', 'w'))
	req := append([]byte{5, cmdConnect, 0, atypIPv6}, remoteAddr.AsSlice()...)
	req = binary.BigEndian.AppendUint16(req, 8)
	tc.Write(req)

	// 方法选择(2) + 认证结果(2) + CONNECT 应答(VER REP RSV ATYP ADDR(16) PORT(2) = 22)
	resp := make([]byte, 2+2+22)
	if _, err := io.ReadFull(tc, resp); err != nil {
		t.Fatal(err)
	}
	if resp[1] != 2 || resp[3] != 0 || resp[5] != repOK || resp[7] != atypIPv6 {
		t.Fatalf("CONNECT 应答异常: %v", resp)
	}

	if _, err := tc.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	if err := tc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(tc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "resp:q" {
		t.Fatalf("got = %q, want %q", got, "resp:q")
	}
}

func TestHTTPProxy(t *testing.T) {
	local, p := setup(t, map[string]string{"alice": "pw"})
	addr := serveProxy(t, p)

	if code, body := get(t, "http://alice:pw@"+addr); code != 200 || !local.Contains(netip.MustParseAddr(body)) {
		t.Fatalf("随机: %d %s", code, body)
	}
	want := local.StickyAddr("alice-session-h1").String()
	if code, body := get(t, "http://alice-session-h1:pw@"+addr); code != 200 || body != want {
		t.Fatalf("session: %d %s，期望 %s", code, body, want)
	}
	if code, body := get(t, "http://alice-ip-2001-db8-1--43:pw@"+addr); code != 200 || body != "2001:db8:1::43" {
		t.Fatalf("显式 IP: %d %s", code, body)
	}
	if code, body := get(t, "http://alice-ip-2001-db8-2--1:pw@"+addr); code != http.StatusForbidden {
		t.Fatalf("池外显式 IP 应当 403: %d %s", code, body)
	}
	if code, body := get(t, "http://alice:wrong@"+addr); code != http.StatusProxyAuthRequired {
		t.Fatalf("错误密码应当 407: %d %s", code, body)
	}
	if code, body := get(t, "http://"+addr); code != http.StatusProxyAuthRequired {
		t.Fatalf("缺少认证应当 407: %d %s", code, body)
	}
}

func TestHTTPConnect(t *testing.T) {
	_, p := setup(t, map[string]string{"alice": "pw"})
	c, err := net.Dial("tcp", serveProxy(t, p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("alice:pw"))
	fmt.Fprintf(c, "CONNECT [2001:db8:ffff::1]:7 HTTP/1.1\r\nHost: [2001:db8:ffff::1]:7\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT 状态码 %d", resp.StatusCode)
	}
	io.WriteString(c, "hello")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo: %q %v", buf, err)
	}
}
