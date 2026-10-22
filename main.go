// he-ipv6-pool：不建网卡，在用户态接入 HE 6in4 隧道，通过 SOCKS5/HTTP 代理以池内任意 IPv6 出站。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

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
	verbose := flag.Bool("v", false, "输出 debug 日志")
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
	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if err := run(*path); err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}
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

func run(path string) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if cfg.Listen == "" {
		return errors.New("配置缺少 listen")
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
	slog.Info("隧道已启动", "server", cfg.ServerIPv4, "prefixes", cfg.Prefixes)
	slog.Info("代理已启动", "listen", l.Addr(), "protocols", "socks5+http")

	errc := make(chan error, 1)
	go func() { errc <- (&proxy.Proxy{Tunnel: t, Users: cfg.Users}).Serve(l) }()
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
