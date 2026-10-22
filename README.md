# he-ipv6-pool

[![release](https://github.com/x6nux/he-ipv6-pool/actions/workflows/release.yml/badge.svg)](https://github.com/x6nux/he-ipv6-pool/actions/workflows/release.yml)
[![license](https://img.shields.io/badge/license-GPL--3.0-blue)](LICENSE)

把一条免费的 [Hurricane Electric](https://tunnelbroker.net) IPv6 隧道变成 **IPv6 代理池**：用 HE 分配的 Routed /64、/48 里的**任意地址**作为出口，一个 /48 就有 2^80 个可用 IP。

- **不建网卡**：不需要 sit/gif/TUN，不改路由表，不影响服务器本身的网络。整个 IPv6 协议栈在用户态运行
- **SOCKS5 + HTTP 同端口**：SOCKS5（含 UDP）和 HTTP 代理（含 CONNECT）共用一个端口，自动识别
- **用户名控制出口**：每次随机，或者按会话固定、设置生存时间、按 /64 轮换，也可以指定具体地址
- **一条命令部署**：`install` 会配置好 systemd 服务（只授予 `CAP_NET_RAW`）和 iptables，随机生成端口和密码
- 支持 Linux（含 1:1 NAT 的云主机），也可以作为 Go 库使用；macOS 可以编译运行，但还没有在真实隧道上验证过

```
客户端 ─SOCKS5/HTTP─→ he-ipv6-pool（gVisor 用户态协议栈）─6in4（IPv4 协议 41）─→ HE 隧道服务器 ─→ IPv6 互联网
```

## 1. 注册 HE 隧道

1. 在 **<https://tunnelbroker.net/register.php>** 免费注册账号，密码会发到邮箱
2. 登录后点击左侧 **Create Regular Tunnel**：
   - **IPv4 Endpoint** 填服务器的公网 IPv4。HE 会从 `66.220.2.74` ping 这个地址，**服务器和云厂商安全组要允许入站 ICMP**，否则会提示 `IP is not ICMP pingable`。必须是真实公网 IP，CGNAT 后面的地址用不了
   - **选择隧道服务器：推荐欧美地区**（理由见下）
3. 创建后打开隧道的 **Tunnel Details** 页面，记下：
   - **Server IPv4 Address**：对应 `-server`
   - **Routed /64**：对应 `-prefix`
   - 点击 **Routed /48** 旁边的 **Assign /48** 再分配一个 /48，也写进 `-prefix`
4. Advanced 页的 **MTU** 保持默认 1480（和本程序的默认值一致）

### 为什么推荐欧美地区的隧道服务器

- **实测**：亚洲节点（新加坡）分配的 /48 在 HE 那一侧一直没有路由，HE 的隧道服务器直接对它回 ICMPv6 不可达；换成美国 Ashburn 节点后，/64 和 /48 立刻全部可用
- **节点多**：HE 的隧道服务器有 30 个在北美和欧洲（北美 18 个、欧洲 12 个），亚洲只有香港、新加坡、东京 3 个。HE 的骨干网和对等互联也主要在这两个地区
- **IP 归属地**：HE 的 IPv6 地址通常按隧道服务器所在地区登记归属地

| 你的服务器在 | 推荐的隧道服务器 |
|---|---|
| 北美 | 离服务器最近的北美节点，例如 Ashburn、New York、Chicago、Dallas、Fremont、Los Angeles、Seattle |
| 欧洲 | Frankfurt、Amsterdam、London、Paris、Zurich、Stockholm 等，选最近的 |
| 亚洲、大洋洲 | 美国西海岸：Fremont、Los Angeles、San Jose、Seattle，是延迟最低的欧美节点 |

创建页面会列出每个隧道服务器的 IPv4，可以先在服务器上 `ping` 一下比较延迟。

## 2. 安装（Linux + systemd）

```bash
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fLo /tmp/he-ipv6-pool https://github.com/x6nux/he-ipv6-pool/releases/latest/download/he-ipv6-pool-linux-$ARCH
chmod +x /tmp/he-ipv6-pool
sudo /tmp/he-ipv6-pool install -server <Server IPv4> -prefix <Routed /64>,<Routed /48>
```

升级到新版本：用同样的方法下载，再执行一次 `sudo /tmp/he-ipv6-pool install`，原有配置保留。

`install` 会把程序装到 `/opt/he-ipv6-pool`，创建系统用户 `hepool` 和 systemd 服务 `he-ipv6-pool`，在 iptables 里放行协议 41 和代理端口（装了 netfilter-persistent 时会持久化），然后启动服务并打印连接信息。端口、用户名、密码没有指定时随机生成。

> **云厂商的安全组**（OCI Security List、AWS Security Group 等）需要自己放行：来自 HE Server IPv4 的**协议 41**，以及代理的 **TCP 端口**。

| 命令 | 作用 |
|---|---|
| `install [选项]` | 安装。用新版二进制再执行一次就是升级，原有配置保留 |
| `config` | 显示当前配置和连接信息 |
| `config -listen :31234`、`config -pass 新密码` 等 | 修改配置，同步防火墙规则并重启服务 |
| `uninstall` | 删除服务、安装目录、用户和对应的 iptables 规则 |

选项：`-server`、`-prefix`（多个用逗号分隔）、`-local`、`-mtu`、`-listen`、`-user`、`-pass`，详见 `install -h` 或 [参数参考](docs/reference.md#12-install--config-的选项)。

### 或者用 Docker Compose

镜像发布在 `ghcr.io/x6nux/he-ipv6-pool`（linux/amd64、linux/arm64，压缩后约 4MB），全部配置通过环境变量传入：

```bash
mkdir he-ipv6-pool && cd he-ipv6-pool
curl -fLO https://raw.githubusercontent.com/x6nux/he-ipv6-pool/main/compose.yaml
curl -fL https://raw.githubusercontent.com/x6nux/he-ipv6-pool/main/.env.example -o .env
vi .env              # 填写 HE_SERVER、HE_PREFIXES、PROXY_LISTEN、PROXY_USER、PROXY_PASS
docker compose up -d
```

| 环境变量 | 说明 |
|---|---|
| `HE_SERVER` | 隧道详情页的 Server IPv4 Address（必填） |
| `HE_PREFIXES` | Routed /64、/48，逗号分隔（必填） |
| `PROXY_LISTEN` | 代理监听地址，例如 `:24601`（必填） |
| `PROXY_USER` / `PROXY_PASS` | 代理用户名和密码（必填），密码可以用 `openssl rand -hex 16` 生成 |
| `HE_LOCAL` / `HE_MTU` | 本地绑定的 IPv4（1:1 NAT 留空）、MTU（默认 1480） |

- **必须用 host 网络**（compose 文件里已经写好）：bridge 网络下 6in4 包要经过 Docker 的 NAT，协议 41 没有端口，宿主机上有别的 6in4 流量或者容器换了 IP 时会因为 NAT 冲突而丢包
- 容器里没有 `install`，宿主机防火墙要自己放行：`iptables -I INPUT -p 41 -s <Server IPv4> -j ACCEPT`，以及代理端口
- 同一台机器上不要同时运行 Docker 版和 `install` 安装的服务：两个实例会互相重置对方的连接

## 3. 使用

```bash
# SOCKS5 和 HTTP 用同一个端口；请用 socks5h://，让代理解析域名
curl -x socks5h://<用户名>:<密码>@<服务器>:<端口> https://api64.ipify.org
curl -x http://<用户名>:<密码>@<服务器>:<端口> https://api64.ipify.org
```

**在用户名后面加后缀来选择出口 IP**，密码不变。后缀顺序任意，可以组合：

| 用户名 | 出口 |
|---|---|
| `alice` | 每条连接随机 |
| `alice-cidr-64` | 每条连接随机，并且每次换一个 /64（用遍所有 /64 之前不重复） |
| `alice-session-abc` | 永久固定一个 IP（服务重启后也不变） |
| `alice-session-abc-ttl-10` | 固定 10 分钟，之后换一个新 IP（`-ttl-` 也可以写 `90s`、`2h`） |
| `alice-session-abc-ttl-30-cidr-64` | 固定 30 分钟，到期后换到一个新的 /64 |
| `alice-ttl-10` | 这个用户的所有连接共用一个 IP，每 10 分钟换一次 |
| `alice-ip-2001-db8-abcd--1` | 指定 `2001:db8:abcd::1`（`:` 写成 `-`），不能和其他后缀组合 |

目标必须有 IPv6 地址（AAAA 记录）。完整规则、会话的过期方式、错误码见 **[docs/reference.md](docs/reference.md)**。

## 4. 手动运行（macOS 或没有 systemd 的系统）

复制 [`config.example.json`](config.example.json) 为 `config.json` 并按隧道详情页填写（字段说明见[参数参考](docs/reference.md#2-配置文件-configjson)），也可以不写配置文件，改用和 compose 相同的[环境变量](docs/reference.md#21-环境变量)。然后：

```bash
sudo setcap cap_net_raw+ep ./he-ipv6-pool   # Linux；macOS 直接用 sudo 运行
./he-ipv6-pool -c config.json               # -v 输出 debug 日志
```

还需要自己放行协议 41：`iptables -I INPUT -p 41 -s <Server IPv4> -j ACCEPT`。在家用路由器后面时，要把协议 41 转发（或 DMZ）到本机，`local_ipv4` 留空或者填内网 IP。

## 5. 作为 Go 库使用

```bash
go get github.com/x6nux/he-ipv6-pool
```

```go
import "github.com/x6nux/he-ipv6-pool/tunnel"

t, err := tunnel.New(tunnel.Config{
	ServerIPv4: netip.MustParseAddr("216.66.80.26"),
	Prefixes:   []netip.Prefix{netip.MustParsePrefix("2001:db8:abcd::/48")},
})
if err != nil {
	log.Fatal(err)
}
defer t.Close()

// 源地址传零值表示随机；也可以传 t.StickyAddr("key")、t.SpreadAddr(64) 或具体地址
client := &http.Client{Transport: &http.Transport{
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return t.Dial(ctx, netip.Addr{}, network, addr)
	},
}}
```

其他 API：`ListenUDP`、`ListenTCP`（在池内地址上接受入站连接）、`Resolve`、`RandomAddr`、`Contains`、`NewWithConn`（在任意 `net.PacketConn` 上运行隧道）。源地址不在池内时返回 `tunnel.ErrNotInPool`，目标没有 IPv6 地址时返回 `tunnel.ErrNoIPv6`，用 `errors.Is` 判断。代理在 `proxy` 包：`(&proxy.Proxy{Tunnel: t, Users: users}).Serve(l)`。

## 6. 排障

| 现象 | 原因和处理 |
|---|---|
| 部分随机连接超时，或者某些会话一直失败 | 有前缀没有被 HE 路由到这条隧道。从任意有 IPv6 的机器 `ping -6` 每个前缀里的地址，已路由的会有回复；或者 `tcpdump -ni <网卡> 'ip proto 41 and host <Server IPv4>'` 看出站包有没有回包 |
| 新分配的 /48 不通 | HE 的新前缀偶尔要过一段时间才生效，可以用 `scripts/check-prefix.sh` 定时检查（见[参数参考](docs/reference.md#6-scriptscheck-prefixsh)）。长时间不通就换一个欧美隧道服务器重建 |
| 所有连接都不通 | 检查安全组是否放行了协议 41；是否同时启用了内核的 HE 隧道（`iface he-ipv6 inet6 v4tunnel`），有的话先 `ifdown he-ipv6`，仍有问题再 `modprobe -r sit` |
| 同一个会话高并发时出现 429 或超时 | 目标网站按 IP 限流。同一个会话的所有请求共用一个 IP，换成不带会话的 `-cidr-64`，或者降低每个会话的并发 |
| 只有 IPv4 的网站打不开 | 本程序只出 IPv6，不会回退到 IPv4 |

## 7. 限制

- 只出 IPv6；目标只有 IPv4 时直接拒绝
- 不支持 SOCKS5 `BIND` 和 UDP 分片。SOCKS5 UDP 使用服务器上的随机端口，只适合本机或同一内网
- 不会自动更新 HE 隧道的 IPv4 端点（动态 IP 可以用 cron 调用 tunnelbroker 的 update URL）
- 管理命令只支持 Linux + systemd；不支持 Windows

## 从源码编译

需要 Go 1.25.5 或更新的 1.25.x：

```bash
go install github.com/x6nux/he-ipv6-pool@latest
# 或者
git clone https://github.com/x6nux/he-ipv6-pool && cd he-ipv6-pool
CGO_ENABLED=0 go build -o he-ipv6-pool .
```

## 发版

GitHub Actions 会运行测试，编译 Linux（amd64/arm64/arm/386/riscv64）和 macOS（amd64/arm64）的二进制，连同 `SHA256SUMS` 发布到 [Releases](https://github.com/x6nux/he-ipv6-pool/releases)：

```bash
gh workflow run release.yml -f version=v0.1.0   # 在 main 最新提交上打标签并发布，也可以在 Actions 页面点 Run workflow
# 或者
git tag v0.1.0 && git push origin v0.1.0
```

版本号格式 `vX.Y.Z`，带后缀（如 `v0.2.0-rc1`）的是预发布版本。用 `he-ipv6-pool -version` 查看版本，升级时下载新版本再执行一次 `install`。详见[参数参考](docs/reference.md#7-发布github-actions)。

## 许可证

[GPL-3.0](LICENSE)
