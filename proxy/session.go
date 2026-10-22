package proxy

import (
	"net/netip"
	"sync"
	"time"
)

// sessionIdle：不带 -ttl- 的有状态会话（带 -cidr-）闲置这么久后才会被清理。
const sessionIdle = 24 * time.Hour

// sessions 记录带 -ttl- 或 -cidr- 的会话选中的地址，零值可用；进程重启后清空。
type sessions struct {
	mu      sync.Mutex
	m       map[string]session
	sweepAt int // 表增长到这个大小时清理一次过期会话，均摊 O(1)
}

type session struct {
	addr    netip.Addr
	expires time.Time
	idle    bool // 没有 ttl：每次使用都把过期时间顺延 sessionIdle
}

// get 返回会话 key 的地址：未过期时复用，否则用 pick 选新地址。ttl 从第一次使用开始计时，
// 期间的使用不会延长它；ttl 为 0 表示闲置 sessionIdle 后才过期。
func (s *sessions) get(key string, ttl time.Duration, pick func() netip.Addr) netip.Addr {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[key]; ok && now.Before(e.expires) {
		if e.idle {
			e.expires = now.Add(sessionIdle)
			s.m[key] = e
		}
		return e.addr
	}
	if s.m == nil {
		s.m = map[string]session{}
	}
	if len(s.m) >= s.sweepAt {
		for k, e := range s.m {
			if !now.Before(e.expires) {
				delete(s.m, k)
			}
		}
		s.sweepAt = max(2*len(s.m), 1024)
	}
	e := session{addr: pick(), expires: now.Add(ttl), idle: ttl == 0}
	if e.idle {
		e.expires = now.Add(sessionIdle)
	}
	s.m[key] = e
	return e.addr
}
