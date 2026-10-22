package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	installDir  = "/opt/he-ipv6-pool"
	serviceName = "he-ipv6-pool"
	serviceUser = "hepool"
)

var (
	installBin  = installDir + "/he-ipv6-pool"
	installConf = installDir + "/config.json"
	unitPath    = "/etc/systemd/system/" + serviceName + ".service"
)

// mgmtFlags 是 install 和 config 共用的参数。
type mgmtFlags struct {
	server, prefixes, local, listen, user, pass string
	mtu                                         int
}

func (f *mgmtFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.server, "server", "", "HE 隧道详情页的 Server IPv4 Address")
	fs.StringVar(&f.prefixes, "prefix", "", "Routed 前缀，多个用逗号分隔，例如 2001:db8:1::/64,2001:db8:abcd::/48")
	fs.StringVar(&f.local, "local", "", "本地绑定的 IPv4，1:1 NAT 的云主机留空")
	fs.IntVar(&f.mtu, "mtu", 1480, "隧道 MTU，与 HE 后台一致")
	fs.StringVar(&f.listen, "listen", "", "代理监听地址（SOCKS5 和 HTTP 共用），默认 :随机高位端口")
	fs.StringVar(&f.user, "user", "", "代理用户名，默认随机生成")
	fs.StringVar(&f.pass, "pass", "", "代理密码，默认随机生成")
}

// setFlags 返回命令行里显式给出的参数名。
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// apply 把命令行里显式给出的参数写进 cfg，没给的保持原值。
// 管理命令只维护一个用户：只改用户名或只改密码时，另一项沿用原值。
func (f *mgmtFlags) apply(cfg *config, set map[string]bool) error {
	if set["server"] {
		a, err := netip.ParseAddr(f.server)
		if err != nil || !a.Is4() {
			return fmt.Errorf("-server 必须是 IPv4 地址，得到 %q", f.server)
		}
		cfg.ServerIPv4 = a
	}
	if set["prefix"] {
		cfg.Prefixes = nil
		for _, s := range strings.Split(f.prefixes, ",") {
			p, err := netip.ParsePrefix(strings.TrimSpace(s))
			if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
				return fmt.Errorf("-prefix 不是 IPv6 前缀: %q", s)
			}
			cfg.Prefixes = append(cfg.Prefixes, p.Masked())
		}
	}
	if set["local"] {
		cfg.LocalIPv4 = netip.Addr{}
		if f.local != "" {
			a, err := netip.ParseAddr(f.local)
			if err != nil || !a.Is4() {
				return fmt.Errorf("-local 必须是 IPv4 地址，得到 %q", f.local)
			}
			cfg.LocalIPv4 = a
		}
	}
	if set["mtu"] {
		cfg.MTU = f.mtu
	}
	if set["listen"] {
		cfg.Listen = f.listen
	}
	if set["user"] || set["pass"] {
		user, pass := firstUser(cfg.Users)
		if set["user"] {
			user = f.user
		}
		if set["pass"] {
			pass = f.pass
		}
		cfg.Users = map[string]string{user: pass}
	}
	return nil
}

// fillDefaults 补齐没有配置的项：MTU 1480、随机高位端口、随机用户名和密码。
func fillDefaults(cfg *config) error {
	if cfg.MTU == 0 {
		cfg.MTU = 1480
	}
	if cfg.Listen == "" {
		port, err := randomPort()
		if err != nil {
			return err
		}
		cfg.Listen = ":" + strconv.Itoa(port)
	}
	if user, pass := firstUser(cfg.Users); len(cfg.Users) <= 1 && (user == "" || pass == "") {
		if user == "" {
			user = "u" + strings.ToLower(crand.Text()[:9]) // base32 字母表，只有字母和数字
		}
		if pass == "" {
			pass = crand.Text() // 26 位 base32，约 130 bit
		}
		cfg.Users = map[string]string{user: pass}
	}
	return nil
}

// firstUser 返回配置里的一个用户，没有用户时返回空串。
func firstUser(users map[string]string) (string, string) {
	for u, p := range users {
		return u, p
	}
	return "", ""
}

// randomPort 在 20000~59999 里挑一个本机空闲的 TCP 端口，跳过 x000、x001 这类整数端口。
func randomPort() (int, error) {
	for range 100 {
		p := 20000 + rand.IntN(40000)
		if p%1000 <= 1 {
			continue
		}
		l, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, errors.New("找不到空闲端口，请用 -listen 指定")
}

// validate 检查管理命令生成或修改后的配置。
func (c config) validate() error {
	switch {
	case !c.ServerIPv4.Is4():
		return errors.New("缺少 -server（HE 隧道详情页的 Server IPv4 Address）")
	case len(c.Prefixes) == 0:
		return errors.New("缺少 -prefix（Routed /64、/48）")
	case c.MTU < 1280 || c.MTU > 65535:
		return fmt.Errorf("MTU %d 超出范围 [1280, 65535]", c.MTU)
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		return fmt.Errorf("-listen 格式应为 [地址]:端口，得到 %q", c.Listen)
	}
	for u, p := range c.Users {
		bad := u == "" || p == "" || strings.ContainsAny(u+p, ":@/ ")
		for _, k := range []string{"-session-", "-ip-", "-ttl-", "-cidr-"} {
			bad = bad || strings.Contains(u, k)
		}
		if bad {
			return fmt.Errorf("用户名 %q 或其密码不合法：不能为空、不能含 : @ / 空格，用户名不能含 -session-、-ip-、-ttl-、-cidr-", u)
		}
	}
	return nil
}

// firewallRules 返回需要放行的 iptables INPUT 规则：HE 服务器发来的协议 41，
// 以及代理端口（监听回环地址时不需要）。
func firewallRules(cfg config) [][]string {
	rules := [][]string{{"-p", "41", "-s", cfg.ServerIPv4.String(), "-j", "ACCEPT"}}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if ip := net.ParseIP(host); err == nil && (ip == nil || !ip.IsLoopback()) {
		rules = append(rules, []string{"-p", "tcp", "-m", "tcp", "--dport", port, "-j", "ACCEPT"})
	}
	return rules
}

// unitFile 生成 systemd 服务：专用用户运行，只授予 CAP_NET_RAW，并开启常用的沙箱选项。
func unitFile() string {
	return fmt.Sprintf(`[Unit]
Description=he-ipv6-pool: userspace HE 6in4 IPv6 proxy pool
After=network-online.target
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
ExecStart=%[2]s -c %[3]s
Restart=on-failure
RestartSec=5
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
LockPersonality=true
SystemCallArchitectures=native
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, serviceUser, installBin, installConf)
}

// ---- 管理命令：install / config / uninstall（Linux + systemd，需要 root） ----

func cmdInstall(args []string) error { return manage("install", args) }
func cmdConfig(args []string) error  { return manage("config", args) }

// manage 实现 install 和 config：读取已安装的配置，用命令行参数覆盖、补齐默认值后写回，
// 同步防火墙并重启服务。install 还会安装二进制、创建用户和 systemd 服务；重复 install 等于升级。
func manage(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var f mgmtFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	install := name == "install"
	old, err := loadConfig(installConf)
	if err != nil && (!install || !errors.Is(err, os.ErrNotExist)) {
		return fmt.Errorf("读取 %s: %w", installConf, err)
	}
	set := setFlags(fs)
	if !install && len(set) == 0 {
		printInfo(old)
		return nil
	}
	cfg := old
	if err := f.apply(&cfg, set); err != nil {
		return err
	}
	if err := fillDefaults(&cfg); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if install {
		if err := ensureUser(); err != nil {
			return err
		}
		if err := os.MkdirAll(installDir, 0o755); err != nil {
			return err
		}
		if err := installBinary(); err != nil {
			return err
		}
		if err := os.WriteFile(unitPath, []byte(unitFile()), 0o644); err != nil {
			return err
		}
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	updateFirewall(old, cfg)
	if err := restartService(); err != nil {
		return err
	}
	printInfo(cfg)
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if err := sh("systemctl", "disable", "--now", serviceName); err != nil {
		fmt.Println("警告:", err)
	}
	os.Remove(unitPath)
	sh("systemctl", "daemon-reload")
	if cfg, err := loadConfig(installConf); err == nil {
		updateFirewall(cfg, config{})
	}
	if err := os.RemoveAll(installDir); err != nil {
		return err
	}
	if _, err := osuser.Lookup(serviceUser); err == nil {
		if err := sh("userdel", serviceUser); err != nil {
			fmt.Println("警告:", err)
		}
	}
	fmt.Printf("已卸载：服务 %s、%s、用户 %s 以及对应的 iptables 规则\n", serviceName, installDir, serviceUser)
	return nil
}

func requireRoot() error {
	if runtime.GOOS != "linux" {
		return errors.New("管理命令只支持 Linux（systemd），其他系统请直接运行 he-ipv6-pool -c config.json")
	}
	if os.Geteuid() != 0 {
		return errors.New("需要 root 权限，请用 sudo 运行")
	}
	return nil
}

// sh 运行外部命令，失败时把命令输出带进错误信息。
func sh(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func ensureUser() error {
	if _, err := osuser.Lookup(serviceUser); err == nil {
		return nil
	}
	shell := "/usr/sbin/nologin"
	if _, err := os.Stat(shell); err != nil {
		shell = "/sbin/nologin"
	}
	return sh("useradd", "--system", "--user-group", "--no-create-home", "--shell", shell, serviceUser)
}

// installBinary 把正在运行的二进制复制到安装目录；先写临时文件再 rename，服务运行中也能替换。
func installBinary() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if self == installBin {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := installBin + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, installBin)
}

// writeConfig 写入配置：root:hepool 0640，服务用户只能读，其他用户看不到密码。
func writeConfig(cfg config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	g, err := osuser.LookupGroup(serviceUser)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	tmp := installConf + ".new"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return err
	}
	if err := os.Chown(tmp, 0, gid); err != nil {
		return err
	}
	return os.Rename(tmp, installConf)
}

// updateFirewall 删除 old 对应的 iptables 规则、插入 cfg 对应的规则并持久化；
// 零值 config 表示没有规则。规则失败只打印警告：防火墙也可能由别的工具管理。
func updateFirewall(old, cfg config) {
	if _, err := exec.LookPath("iptables"); err != nil {
		fmt.Println("提示: 没有 iptables，请自行放行协议 41 和代理端口")
		return
	}
	ipt := func(op string, rule []string) error { return sh("iptables", append([]string{op, "INPUT"}, rule...)...) }
	if old.ServerIPv4.IsValid() {
		for _, r := range firewallRules(old) {
			ipt("-D", r)
		}
	}
	if cfg.ServerIPv4.IsValid() {
		for _, r := range firewallRules(cfg) {
			if ipt("-C", r) == nil {
				continue
			}
			if err := ipt("-I", r); err != nil {
				fmt.Println("警告:", err)
			}
		}
	}
	if _, err := exec.LookPath("netfilter-persistent"); err != nil {
		fmt.Println("提示: 没有 netfilter-persistent，iptables 规则重启后会丢失，请自行持久化")
		return
	}
	if err := sh("netfilter-persistent", "save"); err != nil {
		fmt.Println("警告:", err)
	}
}

// restartService 重启服务并确认它确实跑起来了，失败时附上最近的日志。
func restartService() error {
	for _, args := range [][]string{{"daemon-reload"}, {"enable", serviceName}, {"restart", serviceName}} {
		if err := sh("systemctl", args...); err != nil {
			return err
		}
	}
	time.Sleep(2 * time.Second)
	if sh("systemctl", "is-active", "--quiet", serviceName) != nil {
		out, _ := exec.Command("journalctl", "-u", serviceName, "-n", "20", "--no-pager").CombinedOutput()
		return fmt.Errorf("服务没有正常运行，最近的日志:\n%s", out)
	}
	return nil
}

func printInfo(cfg config) {
	host, port, _ := net.SplitHostPort(cfg.Listen)
	if host == "" {
		host = "<服务器IP>"
	}
	addr := net.JoinHostPort(host, port)
	user, pass := firstUser(cfg.Users)
	fmt.Printf(`版本      %s
配置文件  %s
HE 服务器 %s
前缀      %v
监听      %s（SOCKS5 和 HTTP 共用）
用户名    %s
密码      %s

  curl -x socks5h://%[6]s:%[7]s@%[8]s https://api64.ipify.org
  curl -x http://%[6]s:%[7]s@%[8]s https://api64.ipify.org

用户名后缀选择出口（可以组合）：-session-<id> 固定 IP，-ttl-<分钟> 固定 IP 的生存时间，
-cidr-64 按 /64 轮换；-ip-<地址> 指定 IP（地址里的 : 写成 -）。
云厂商的安全组还需要放行协议 41 和 TCP %[9]s。
`, versionString(), installConf, cfg.ServerIPv4, cfg.Prefixes, cfg.Listen, user, pass, addr, port)
}
