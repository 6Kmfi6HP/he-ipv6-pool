package main

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestMgmtConfig(t *testing.T) {
	f := mgmtFlags{server: "216.66.80.26", prefixes: "2001:db8:1::/64, 2001:db8:abcd::/48"}
	var cfg config
	if err := f.apply(&cfg, map[string]bool{"server": true, "prefix": true}); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerIPv4 != netip.MustParseAddr("216.66.80.26") || len(cfg.Prefixes) != 2 || cfg.Prefixes[1] != netip.MustParsePrefix("2001:db8:abcd::/48") {
		t.Fatalf("apply 结果不对: %+v", cfg)
	}

	// 默认值：随机高位端口、随机用户名和密码，并且能通过校验
	if err := fillDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := net.LookupPort("tcp", port); err != nil || host != "" || n < 20000 || n >= 60000 {
		t.Fatalf("随机端口不对: %q", cfg.Listen)
	}
	user, pass := firstUser(cfg.Users)
	if len(cfg.Users) != 1 || len(user) < 8 || len(pass) < 20 || strings.ContainsAny(user+pass, ":@/-") {
		t.Fatalf("随机凭据不对: %q %q", user, pass)
	}
	if cfg.MTU != 1480 {
		t.Fatalf("默认 MTU = %d", cfg.MTU)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}

	// 只改密码时保留用户名；只改端口时凭据不变
	if err := (&mgmtFlags{pass: "NewPassw0rd"}).apply(&cfg, map[string]bool{"pass": true}); err != nil {
		t.Fatal(err)
	}
	if u, p := firstUser(cfg.Users); u != user || p != "NewPassw0rd" {
		t.Fatalf("只改密码后: %q %q", u, p)
	}
	if err := (&mgmtFlags{listen: ":31234"}).apply(&cfg, map[string]bool{"listen": true}); err != nil {
		t.Fatal(err)
	}
	if u, p := firstUser(cfg.Users); cfg.Listen != ":31234" || u != user || p != "NewPassw0rd" {
		t.Fatalf("只改端口后: %+v", cfg)
	}

	// 非法参数
	bad := []struct {
		name string
		f    mgmtFlags
		set  string
	}{
		{"IPv6 server", mgmtFlags{server: "2001:db8::1"}, "server"},
		{"IPv4 prefix", mgmtFlags{prefixes: "10.0.0.0/8"}, "prefix"},
		{"IPv6 local", mgmtFlags{local: "2001:db8::1"}, "local"},
	}
	for _, c := range bad {
		var empty config
		if err := c.f.apply(&empty, map[string]bool{c.set: true}); err == nil {
			t.Errorf("%s 应当报错", c.name)
		}
	}
	// 会破坏 URL 或用户名选择器的用户名必须被拒绝
	for _, u := range []string{"a:b", "a@b", "x-ip-y", "x-session-y", "x-cidr-y", "x-ttl-y"} {
		c := cfg
		c.Users = map[string]string{u: "pw"}
		if c.validate() == nil {
			t.Errorf("用户名 %q 应当被拒绝", u)
		}
	}
	if (config{Listen: ":1", Users: map[string]string{"u": "p"}}).validate() == nil {
		t.Error("缺少 server 和 prefix 时应当报错")
	}
}

func TestFirewallRules(t *testing.T) {
	cfg := config{ServerIPv4: netip.MustParseAddr("216.66.80.26"), Listen: ":24601"}
	want := [][]string{
		{"-p", "41", "-s", "216.66.80.26", "-j", "ACCEPT"},
		{"-p", "tcp", "-m", "tcp", "--dport", "24601", "-j", "ACCEPT"},
	}
	if got := firewallRules(cfg); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("firewallRules = %v", got)
	}
	cfg.Listen = "127.0.0.1:1080"
	if got := firewallRules(cfg); len(got) != 1 {
		t.Fatalf("监听回环地址时不应放行代理端口: %v", got)
	}
}
