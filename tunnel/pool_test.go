package tunnel

import (
	"net/netip"
	"testing"
)

func TestPool(t *testing.T) {
	p := pool{netip.MustParsePrefix("2001:db8:1::/48"), netip.MustParsePrefix("2001:db8:2:3::/64")}
	hits := map[netip.Prefix]int{}
	for range 1000 {
		a := p.random()
		if !p.contains(a) {
			t.Fatalf("random %s 不在池内", a)
		}
		for _, pfx := range p {
			if pfx.Contains(a) {
				hits[pfx]++
			}
		}
	}
	if len(hits) != 2 {
		t.Fatalf("random 没有覆盖所有前缀: %v", hits)
	}
	if a, b := p.random(), p.random(); a == b {
		t.Fatal("random 连续两次结果相同")
	}

	a, b := p.sticky("alice-session-1"), p.sticky("alice-session-1")
	if a != b || !p.contains(a) {
		t.Fatalf("sticky 不稳定或越界: %s %s", a, b)
	}
	if p.sticky("alice-session-2") == a {
		t.Fatal("不同 key 得到了相同地址")
	}
	if p.contains(netip.MustParseAddr("2001:db8:2:4::1")) {
		t.Fatal("contains 误判池外地址")
	}

	// 非字节对齐的前缀
	odd := pool{netip.MustParsePrefix("2001:db8:ab80::/41")}
	for range 1000 {
		if a := odd.random(); !odd.contains(a) {
			t.Fatalf("random %s 不在 /41 内", a)
		}
	}
}

func TestFill(t *testing.T) {
	var ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	got := fill(netip.MustParsePrefix("2001:db8:ab80::/41"), ones)
	want := netip.MustParseAddr("2001:db8:abff:ffff:ffff:ffff:ffff:ffff")
	if got != want {
		t.Fatalf("fill = %s, want %s", got, want)
	}
}

func TestSpread(t *testing.T) {
	p := pool{netip.MustParsePrefix("2001:db8:1::/48"), netip.MustParsePrefix("2001:db8:2:3::/64")}
	// 按 /64 轮换：池内共 65536+1 个 /64，连续这么多次都不重复，并且都在池内
	distinct := func(p pool, cidr int, n uint64) {
		t.Helper()
		seen := map[netip.Prefix]bool{}
		for i := range n {
			a := p.spread(cidr, 12345+i)
			s := netip.PrefixFrom(a, cidr).Masked()
			if !p.contains(a) || seen[s] {
				t.Fatalf("/%d 第 %d 次: %s（在池内 %v，子网重复 %v）", cidr, i, a, p.contains(a), seen[s])
			}
			seen[s] = true
		}
	}
	distinct(p, 64, 1<<16+1)
	distinct(pool{netip.MustParsePrefix("2001:db8:ab80::/41")}, 44, 8)  // 非字节对齐
	distinct(pool{netip.MustParsePrefix("2001:db8:2:3::/64")}, 72, 256) // 比 /64 更细

	if a, b := p.spread(64, 7), p.spread(64, 7); a == b {
		t.Fatal("同一个子网里的主机位应当随机")
	}
	// 粒度比前缀还粗时退化为前缀内随机
	for i := range uint64(100) {
		if a := p.spread(32, i); !p.contains(a) {
			t.Fatalf("spread(32) = %s 不在池内", a)
		}
	}
}
