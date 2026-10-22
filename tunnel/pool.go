package tunnel

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
	"math/rand/v2"
	"net/netip"
)

// pool 是可用作源地址的 IPv6 前缀集合，元素均已 Masked。
type pool []netip.Prefix

// random 先均匀选一个前缀，再随机填充主机位。
func (p pool) random() netip.Addr {
	var host [16]byte
	binary.BigEndian.PutUint64(host[:8], rand.Uint64())
	binary.BigEndian.PutUint64(host[8:], rand.Uint64())
	return fill(p[rand.IntN(len(p))], host)
}

// sticky 把 key 哈希成固定地址：同一个 key 永远得到同一个结果，无需保存状态。
func (p pool) sticky(key string) netip.Addr {
	h := sha256.Sum256([]byte(key))
	pfx := p[binary.BigEndian.Uint64(h[:8])%uint64(len(p))]
	return fill(pfx, [16]byte(h[16:]))
}

// spread 返回第 i 个轮换地址：把池拆成 /cidr 子网并编号，第 i 次取编号 i*m mod n 的子网，主机位随机。
// n 是子网总数，m 与 n 互质，因此乘法取模是 [0,n) 上的置换：连续 n 个 i 落在 n 个不同的子网，
// 用遍所有子网之前不会重复；m 取黄金比例常数，相邻两次落在相距很远的子网，而不是挨着的 ::1、::2。
// 比 cidr 更长（更小）的前缀算一个子网。
func (p pool) spread(cidr int, i uint64) netip.Addr {
	var n uint64
	for _, pfx := range p {
		n += subnets(pfx, cidr)
	}
	m := 0x9E3779B97F4A7C15 % n
	for gcd(m, n) != 1 {
		m++
	}
	hi, lo := bits.Mul64(i%n, m)
	idx := bits.Rem64(hi, lo, n)
	for _, pfx := range p {
		if k := subnets(pfx, cidr); idx >= k {
			idx -= k
			continue
		}
		// 地址的前 cidr 位换成子网编号（右对齐），其余主机位随机；前缀位随后由 fill 覆盖
		h, l := rand.Uint64(), rand.Uint64()
		if s := uint(128 - min(max(cidr, pfx.Bits()), 128)); s >= 64 {
			h = h&(1<<(s-64)-1) | idx<<(s-64)
		} else {
			h, l = idx>>(64-s), l&(1<<s-1)|idx<<s
		}
		var host [16]byte
		binary.BigEndian.PutUint64(host[:8], h)
		binary.BigEndian.PutUint64(host[8:], l)
		return fill(pfx, host)
	}
	panic("unreachable")
}

// subnets 返回 pfx 包含的 /cidr 子网个数。
// ponytail: 上限 2^40 个，超出部分的高位子网不参与轮换；HE 最大分配 /48，按 /64 才 2^16 个
func subnets(pfx netip.Prefix, cidr int) uint64 {
	return 1 << min(max(cidr-pfx.Bits(), 0), 40)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (p pool) contains(a netip.Addr) bool {
	for _, pfx := range p {
		if pfx.Contains(a) {
			return true
		}
	}
	return false
}

// fill 保留 pfx 的前缀位，主机位取自 host。
func fill(pfx netip.Prefix, host [16]byte) netip.Addr {
	b := pfx.Addr().As16()
	for i := range b {
		n := pfx.Bits() - i*8 // 本字节中属于前缀的位数
		var mask byte
		switch {
		case n >= 8:
			mask = 0xff
		case n > 0:
			mask = ^byte(0xff >> n)
		}
		b[i] = b[i]&mask | host[i]&^mask
	}
	return netip.AddrFrom16(b)
}
