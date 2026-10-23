// he-ipv6-pool：不建网卡，在用户态接入 HE 6in4 隧道，通过 SOCKS5/HTTP 代理以池内任意 IPv6 出站。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"github.com/x6nux/he-ipv6-pool/proxy"
	"github.com/x6nux/he-ipv6-pool/tunnel"
)

// version 由发布流程通过 -ldflags "-X main.version=<tag>" 注入；为空时用 Go 的构建信息，
// 即 go install …@v0.1.0 的模块版本，或者源码编译时的提交号（如 v0.0.0-20261022…-abcdef+dirty）。
var version string

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

type config struct {
	ServerIPv4 netip.Addr        `json:"server_ipv4"`
	LocalIPv4  netip.Addr        `json:"local_ipv4"`
	Prefixes   []netip.Prefix    `json:"prefixes"`
	MTU        int               `json:"mtu"`
	Listen     string            `json:"listen"` // SOCKS5 和 HTTP 共用的监听地址
	Users      map[string]string `json:"users"`
}

func main() {
	if len(os.Args) > 1 {
		cmds := map[string]func([]string) error{"install": cmdInstall, "config": cmdConfig, "uninstall": cmdUninstall}
		if cmd, ok := cmds[os.Args[1]]; ok {
			if err := cmd(os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(os.Stderr, "错误:", err)
				os.Exit(1)
			}
			return
		}
	}
	path := flag.String("c", "config.json", "配置文件路径")
	verbose := flag.Bool("v", false, "输出 debug 日志（每个请求一行），等同于环境变量 LOG_LEVEL=debug")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), `用法:
  he-ipv6-pool [-c config.json] [-v]                              前台运行
  he-ipv6-pool -version                                           显示版本
  he-ipv6-pool install -server <HE IPv4> -prefix <前缀[,前缀]>    安装为 systemd 服务
  he-ipv6-pool config [选项]                                      查看或修改已安装服务的配置
  he-ipv6-pool uninstall                                          卸载
子命令加 -h 查看选项。

`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(versionString())
		return
	}
	if err := setupLog(*verbose); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	if err := run(*path); err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}
}

// setupLog 按 -v 或环境变量 LOG_LEVEL（debug、info、warn、error）设置日志级别，-v 优先；
// 在 systemd 下运行时（journald 会设置 JOURNAL_STREAM）不输出时间，journald 自己会记录。
func setupLog(verbose bool) error {
	if os.Getenv("JOURNAL_STREAM") != "" {
		log.SetFlags(0)
	}
	level := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			return fmt.Errorf("环境变量 LOG_LEVEL=%q 无效，可选 debug、info、warn、error", v)
		}
	}
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetLogLoggerLevel(level)
	return nil
}

func loadConfig(path string) (config, error) {
	var cfg config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return cfg, nil
}

// envVars 是环境变量与 install 选项的对应关系；环境变量覆盖配置文件里的同名项。
var envVars = []struct{ name, flag string }{
	{"HE_SERVER", "server"}, {"HE_PREFIXES", "prefix"}, {"HE_LOCAL", "local"}, {"HE_MTU", "mtu"},
	{"PROXY_LISTEN", "listen"}, {"PROXY_USER", "user"}, {"PROXY_PASS", "pass"},
}

// applyEnv 用环境变量覆盖 cfg，解析和校验与 install 的同名选项相同；返回是否设置了任何变量。
func applyEnv(cfg *config) (bool, error) {
	used := false
	for _, e := range envVars {
		v, ok := os.LookupEnv(e.name)
		if !ok {
			continue
		}
		used = true
		// apply 只读取 set 里那一项对应的字段，所以字符串字段都填 v
		f := mgmtFlags{server: v, prefixes: v, local: v, listen: v, user: v, pass: v}
		if e.flag == "mtu" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return used, fmt.Errorf("环境变量 %s=%q 不是整数", e.name, v)
			}
			f.mtu = n
		}
		if err := f.apply(cfg, map[string]bool{e.flag: true}); err != nil {
			return used, fmt.Errorf("环境变量 %s: %w", e.name, err)
		}
	}
	return used, nil
}

// run 读取配置文件并用环境变量覆盖；配置文件不存在但设置了环境变量时只用环境变量（适合容器部署）。
func run(path string) error {
	cfg, err := loadConfig(path)
	noFile := errors.Is(err, os.ErrNotExist)
	if err != nil && !noFile {
		return err
	}
	used, envErr := applyEnv(&cfg)
	switch {
	case envErr != nil:
		return envErr
	case noFile && !used:
		return fmt.Errorf("%w（也可以用环境变量 HE_SERVER、HE_PREFIXES、PROXY_LISTEN 等配置）", err)
	case !cfg.ServerIPv4.Is4():
		return errors.New("缺少 server_ipv4（环境变量 HE_SERVER）")
	case len(cfg.Prefixes) == 0:
		return errors.New("缺少 prefixes（环境变量 HE_PREFIXES）")
	case cfg.Listen == "":
		return errors.New("缺少 listen（环境变量 PROXY_LISTEN）")
	}
	for u, p := range cfg.Users {
		if u == "" || p == "" {
			return errors.New("代理用户名和密码都不能为空（环境变量 PROXY_USER、PROXY_PASS）")
		}
	}
	t, err := tunnel.New(tunnel.Config{ServerIPv4: cfg.ServerIPv4, LocalIPv4: cfg.LocalIPv4, Prefixes: cfg.Prefixes, MTU: cfg.MTU})
	if err != nil {
		return err
	}
	defer t.Close()
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer l.Close() // 先于 t.Close 执行（defer 后进先出）
	if len(cfg.Users) == 0 && !l.Addr().(*net.TCPAddr).IP.IsLoopback() {
		slog.Warn("监听非回环地址且没有配置 users，任何人都可以使用这个代理", "listen", l.Addr())
	}
	slog.Info("已启动", startupAttrs(cfg, l.Addr())...)

	p := &proxy.Proxy{Tunnel: t, Users: cfg.Users}
	errc := make(chan error, 1)
	go func() { errc <- p.Serve(l) }()
	go checkTunnel(t, cfg.ServerIPv4)
	go logStats(p, statsInterval)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		slog.Info("收到信号，退出", "signal", s)
		return nil
	case err := <-errc:
		return err
	}
}

func startupAttrs(cfg config, listen net.Addr) []any {
	local, mtu, auth := "自动", cfg.MTU, "关闭"
	if cfg.LocalIPv4.IsValid() {
		local = cfg.LocalIPv4.String()
	}
	if mtu == 0 {
		mtu = 1480
	}
	if n := len(cfg.Users); n > 0 {
		auth = fmt.Sprintf("%d 个用户", n)
	}
	return []any{"version", versionString(), "server", cfg.ServerIPv4, "local", local, "mtu", mtu,
		"prefixes", cfg.Prefixes, "listen", listen, "protocols", "socks5+http", "auth", auth}
}

// checkTargets 是启动检查连接的目标：Cloudflare 和 Google 公共 DNS 的 IPv6 任播地址，长期稳定。
var checkTargets = []netip.AddrPort{
	netip.MustParseAddrPort("[2606:4700:4700::1111]:443"),
	netip.MustParseAddrPort("[2001:4860:4860::8888]:443"),
}

// checkTunnel 检查每个前缀能否收到回包。结果只写日志，不影响服务运行：HE 临时故障时
// 退出会让 systemd 不停重启；也不剔除不通的前缀，否则所有会话的固定 IP 都会变。
func checkTunnel(t *tunnel.Tunnel, server netip.Addr) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := 0
	for _, r := range t.Check(ctx, checkTargets) {
		if r.Err != nil {
			slog.Warn("隧道检查失败：这个前缀收不到回包，可能还没有被 HE 路由到这条隧道", "prefix", r.Prefix, "src", r.Src, "err", r.Err)
			continue
		}
		ok++
		slog.Info("隧道检查通过", "prefix", r.Prefix, "src", r.Src, "rtt", r.RTT.Round(time.Millisecond))
	}
	if ok == 0 {
		slog.Error("所有前缀都不通：请确认防火墙和云厂商安全组放行了来自 HE 的协议 41，"+
			"HE 后台填写的 IPv4 端点是本机的公网 IP，并且没有同时启用内核的 HE 隧道", "server", server)
	}
}

const statsInterval = 10 * time.Minute

// logStats 每隔 interval 输出一次这段时间的请求统计；没有任何活动时不输出。
func logStats(p *proxy.Proxy, interval time.Duration) {
	var prev proxy.Stats
	for range time.Tick(interval) {
		s := p.Stats()
		d := s.Sub(prev)
		prev = s
		if d.Requests() == 0 && s.Active == 0 {
			continue
		}
		slog.Info(fmt.Sprintf("最近 %d 分钟", int(interval.Minutes())), d.Attrs()...)
	}
}
