# he-ipv6-pool 参数参考

本文逐项说明所有命令行参数、配置字段、用户名后缀、协议行为和错误码。入门和原理见 [README](../README.md)。
示例中的 `203.0.113.10`、`alice`、`change-me` 都是占位值。

- [1. 命令行](#1-命令行)
- [2. 配置文件](#2-配置文件-configjson)
- [3. 用户名后缀：选择出口 IP](#3-用户名后缀选择出口-ip)
- [4. 代理协议](#4-代理协议)
- [5. 错误码速查](#5-错误码速查)
- [6. scripts/check-prefix.sh](#6-scriptscheck-prefixsh)
- [7. 发布](#7-发布github-actions)
- [8. 限制](#8-限制)

---

## 1. 命令行

```
he-ipv6-pool [-c config.json] [-v]                              前台运行
he-ipv6-pool -version                                           显示版本
he-ipv6-pool install -server <HE IPv4> -prefix <前缀[,前缀]>    安装为 systemd 服务
he-ipv6-pool config [选项]                                      查看或修改已安装服务的配置
he-ipv6-pool uninstall                                          卸载
```

子命令都可以加 `-h` 查看选项。

### 1.1 前台运行

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-c <路径>` | `config.json` | 配置文件路径，格式见[第 2 节](#2-配置文件-configjson) |
| `-v` | 关 | 输出 debug 日志（每个失败连接的原因等） |
| `-version` | — | 打印版本号后退出。发布版是标签名（如 `v1.0.0`），自己编译的是 `dev` |

需要 root 或 `CAP_NET_RAW`（推荐 `sudo setcap cap_net_raw+ep ./he-ipv6-pool` 后用普通用户运行）。收到 SIGINT 或 SIGTERM 时关闭监听和隧道后退出。

### 1.2 install / config 的选项

两个命令的选项相同。**只有命令行里显式给出的选项才会修改配置**，没给的保持原值。

| 选项 | 对应配置字段 | 默认值 | 说明 |
|---|---|---|---|
| `-server <IPv4>` | `server_ipv4` | 无，首次安装必填 | HE 隧道详情页的 Server IPv4 Address |
| `-prefix <前缀,…>` | `prefixes` | 无，首次安装必填 | Routed /64、/48，多个用逗号分隔，例如 `2001:db8:1::/64,2001:db8:abcd::/48`。给出时整体替换原有列表 |
| `-local <IPv4>` | `local_ipv4` | 空 | 本地绑定的 IPv4。1:1 NAT 的云主机留空；`-local ""` 清空 |
| `-mtu <数字>` | `mtu` | `1480` | 隧道 MTU，范围 1280~65535，要和 HE 后台一致 |
| `-listen <[地址]:端口>` | `listen` | `:随机端口` | 代理监听地址，SOCKS5 和 HTTP 共用 |
| `-user <用户名>` | `users` | 随机 | 代理用户名 |
| `-pass <密码>` | `users` | 随机 | 代理密码 |

管理命令只维护一个用户：只给 `-user` 时沿用原密码，只给 `-pass` 时沿用原用户名。

**没有配置的项会自动生成：**

| 项 | 规则 |
|---|---|
| 端口 | 20000~59999 之间本机空闲的 TCP 端口，跳过 x000、x001 这类整数端口 |
| 用户名 | `u` + 9 位小写字母和数字 |
| 密码 | 26 位 base32（大写字母和 2~7），约 130 bit |

**校验**（不通过时报错退出，不做任何改动）：

- `-server` 必须是 IPv4，`-prefix` 每一项都必须是 IPv6 前缀，`-local` 为空或 IPv4
- `listen` 必须是 `[地址]:端口` 格式
- 用户名和密码不能为空，不能含 `:`、`@`、`/`、空格（会破坏代理 URL）
- 用户名不能含 `-session-`、`-ip-`、`-ttl-`、`-cidr-`（会和[用户名后缀](#3-用户名后缀选择出口-ip)冲突）

### 1.3 install

只支持 Linux + systemd，需要 root。依次执行：

1. 读取已有的 `/opt/he-ipv6-pool/config.json`（如果有），用命令行选项覆盖，补齐默认值并校验
2. 创建系统用户和组 `hepool`（不能登录、没有家目录）
3. 把当前运行的二进制复制到 `/opt/he-ipv6-pool/he-ipv6-pool`（先写临时文件再改名，服务运行中也能替换）
4. 写入 systemd 服务 `/etc/systemd/system/he-ipv6-pool.service`
5. 写入配置，权限 `root:hepool 0640`（服务能读，其他普通用户看不到密码）
6. 同步 iptables 规则并持久化，见 [1.6](#16-防火墙规则)
7. `systemctl enable` + `restart`，等 2 秒确认服务在运行，失败时打印最近 20 行日志
8. 打印配置和连接示例

**重复执行 install 就是升级**：用新版二进制执行 `sudo ./he-ipv6-pool-新版 install`，原有配置全部保留。

### 1.4 config

| 用法 | 作用 |
|---|---|
| `config` | 打印当前配置和连接示例，不做改动 |
| `config <选项>` | 修改配置，同步防火墙规则，重启服务 |

例如：`config -listen :31234` 换端口（旧端口的规则会删掉），`config -pass 新密码` 改密码，`config -prefix 新前缀` 换前缀。还没安装时会报错。

### 1.5 uninstall

停止并禁用服务，然后删除服务文件、`/opt/he-ipv6-pool`（包括配置）、用户 `hepool`，以及配置里对应的 iptables 规则（并持久化）。

### 1.6 防火墙规则

install 和 config 会在 iptables `INPUT` 链最前面插入（已存在则跳过）：

```
-p 41 -s <server_ipv4> -j ACCEPT                    # 6in4 隧道
-p tcp -m tcp --dport <listen 的端口> -j ACCEPT     # 代理端口；监听回环地址时不加
```

- 修改配置时先删除旧配置对应的规则，再添加新规则
- 装了 `netfilter-persistent` 时执行 `netfilter-persistent save` 持久化，否则只提示
- 没有 iptables 时只打印提示
- 云厂商的安全组（OCI Security List、AWS Security Group 等）需要自己放行协议 41 和代理端口

### 1.7 systemd 服务

以 `hepool` 用户运行，`ExecStart=/opt/he-ipv6-pool/he-ipv6-pool -c /opt/he-ipv6-pool/config.json`，异常退出 5 秒后重启。权限只有 `CAP_NET_RAW`，并开启了 `NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、`PrivateTmp`、`PrivateDevices`、`ProtectKernel*`、`RestrictAddressFamilies` 等沙箱选项，`LimitNOFILE=65536`。

```bash
systemctl status he-ipv6-pool      # 状态
journalctl -u he-ipv6-pool -f      # 日志
```

### 1.8 Docker

镜像 `ghcr.io/x6nux/he-ipv6-pool`，支持 linux/amd64 和 linux/arm64，基于 `scratch`，里面只有一个静态二进制。默认命令是 `-c /etc/he-ipv6-pool/config.json`，把配置文件挂载到这个位置即可。

| 镜像标签 | 说明 |
|---|---|
| `latest` | 最新正式版 |
| `0.2.0` | 指定版本（标签名去掉 `v`） |
| `0.2` | 0.2 系列的最新版本 |
| `0.3.0-rc1` 等 | 预发布版本只打完整版本号，不会更新 `latest` 和 `0.3` |

```bash
docker run -d --name he-ipv6-pool --restart unless-stopped \
  --network host --cap-drop ALL --cap-add NET_RAW --read-only \
  -v /etc/he-ipv6-pool/config.json:/etc/he-ipv6-pool/config.json:ro \
  ghcr.io/x6nux/he-ipv6-pool:latest

docker logs -f he-ipv6-pool                              # 日志
docker run --rm ghcr.io/x6nux/he-ipv6-pool:latest -version
```

| 参数 | 作用 |
|---|---|
| `--network host` | **必需**。直接使用宿主机网络，6in4 包不经过 NAT |
| `--cap-drop ALL --cap-add NET_RAW` | 只保留 raw socket 需要的权限 |
| `--read-only` | 程序不写文件，根文件系统可以只读 |
| `-v …:ro` | 挂载配置，只读即可 |

**为什么不能用 bridge 网络**：容器发出的协议 41 包要经过 Docker 的 MASQUERADE。协议 41 没有端口，conntrack 只按「本机 IP ↔ HE 服务器 IP」区分连接，同一对地址只能有一条记录。宿主机自己的 6in4 流量、上一个容器留下的记录（容器重建后 IP 变了）都会占用这个组合，新的包就会因为 NAT 冲突被丢弃。这条记录空闲 10 分钟才会过期，而 HE 不断发来的回包会一直刷新它。实测中 bridge 网络在宿主机刚停掉 6in4 服务后完全不通，手动 `conntrack -D -p 41` 删除旧记录后才恢复。

**其他注意事项：**

- 容器里没有管理命令。防火墙要在宿主机上自己放行：`iptables -I INPUT -p 41 -s <server_ipv4> -j ACCEPT`，以及 `listen` 的 TCP 端口
- 同一台机器上不要同时运行 Docker 版和 `install` 安装的 systemd 服务：两个实例会同时收到所有协议 41 的包，各自的协议栈会把对方的连接当成未知连接并重置。从 systemd 换到 Docker 时先执行 `uninstall`，或者 `systemctl disable --now he-ipv6-pool`
- 自己构建：`docker build --build-arg VERSION=v0.2.0 -t he-ipv6-pool .`；多架构：`docker buildx build --platform linux/amd64,linux/arm64 …`

---

## 2. 配置文件 config.json

```json
{
  "server_ipv4": "216.66.80.26",
  "local_ipv4": "",
  "prefixes": ["2001:db8:1234::/64", "2001:db8:abcd::/48"],
  "mtu": 1480,
  "listen": "127.0.0.1:1080",
  "users": {"alice": "change-me"}
}
```

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `server_ipv4` | 字符串 | 是 | — | HE 隧道详情页的 Server IPv4 Address。只接受来自这个地址的 6in4 包 |
| `local_ipv4` | 字符串 | 否 | 空 | raw socket 绑定的本地 IPv4，空表示由内核选择。1:1 NAT 云主机留空；家宽路由器后面留空或填内网 IP |
| `prefixes` | 字符串数组 | 是 | — | 可用作出口的 IPv6 前缀，只能填 HE 已经路由到这条隧道的 Routed /64、/48。主机位会自动清零 |
| `mtu` | 整数 | 否 | `1480` | 隧道 MTU，范围 1280~65535 |
| `listen` | 字符串 | 是 | — | 代理监听地址，例如 `:24601`（所有地址）、`127.0.0.1:1080`（仅本机）。SOCKS5 和 HTTP 共用 |
| `users` | 对象 | 否 | 空 | 用户名 → 密码。为空时不要求认证，这时只应该监听回环地址，否则启动时会打印警告 |

手动编辑配置时可以写多个用户；管理命令只维护其中一个。基础用户名不要包含 `-session-`、`-ip-`、`-ttl-`、`-cidr-`，否则会被当成后缀拆开，导致认证失败。

---

## 3. 用户名后缀：选择出口 IP

出口 IP 由**用户名后面的后缀**决定，密码不变。SOCKS5 和 HTTP 代理的规则完全相同。

```
<用户名>[-session-<id>][-ttl-<时长>][-cidr-<位数>]     后缀顺序任意，可以组合
<用户名>-ip-<地址>                                     指定地址，不能和其他后缀组合
```

### 3.1 后缀

| 后缀 | 值 | 作用 |
|---|---|---|
| （无） | — | 每条连接随机：先在 `prefixes` 里等概率选一个前缀，再随机填充主机位 |
| `-session-<id>` | 任意字符串，可以带 `-`，可以为空 | 同一个会话固定同一个 IP |
| `-ttl-<时长>` | 纯数字按**分钟**；也可以写 `90s`、`10m`、`2h`、`1h30m`；必须大于 0 | 固定 IP 的生存时间，从**第一次使用**开始计时，期间的使用不会延长它，到期后换一个新 IP |
| `-cidr-<位数>` | 1~128 的整数 | 按 /位数 子网轮换，见 [3.3](#33--cidr--子网轮换) |
| `-ip-<地址>` | IPv6 地址，`:` 写成 `-`，例如 `2001-db8-abcd--1`；SOCKS5 也可以直接写冒号 | 指定出口地址，必须在 `prefixes` 内 |

### 3.2 组合效果

| 用户名示例 | 出口 | 状态 |
|---|---|---|
| `alice` | 每条连接随机 | 无 |
| `alice-cidr-64` | 每条连接随机，每次换一个 /64 | 无 |
| `alice-session-abc` | 永久固定 | 无状态：对 `alice-session-abc` 做哈希映射，只要 `prefixes` 的数量和顺序不变，重启后也不变 |
| `alice-session-abc-ttl-10` | 固定 10 分钟，之后换新 IP | 内存 |
| `alice-session-abc-cidr-64` | 固定 IP，不同会话保证落在不同的 /64 | 内存 |
| `alice-session-abc-ttl-30-cidr-64` | 固定 30 分钟，到期后换到一个新的 /64 | 内存 |
| `alice-ttl-10` | 不带会话：这个用户的所有连接共用一个 IP，每 10 分钟换一次 | 内存 |
| `alice-ttl-10-cidr-64` | 同上，每次换到新的 /64 | 内存 |
| `alice-ip-2001-db8-abcd--1` | 固定为 `2001:db8:abcd::1` | 无 |

**内存中的会话：**

- 会话由「基础用户名 + 会话 ID」标识。同一个会话 ID 换了 `-ttl-` 或 `-cidr-` 的值，仍然是同一个会话；`-ttl-` 的值只在分配新 IP 时生效
- 单独的 `-session-<id>` 走哈希映射，和带 `-ttl-`、`-cidr-` 的同名会话互不相干：`alice-session-abc` 和 `alice-session-abc-ttl-10` 是两个不同的 IP
- 服务重启后清空，重新分配
- 带 `-ttl-` 的会话到期后失效；不带 `-ttl-` 的（只带 `-cidr-`）闲置 24 小时后失效，期间每次使用都会顺延
- 过期会话在会话表增长时顺带清理，不会一直占用内存
- 正在使用的连接不受到期影响，到期后新建的连接才会换 IP

### 3.3 -cidr- 子网轮换

把所有前缀拆成 /位数 的子网并统一编号。例如 `-cidr-64` 时，/48 拆成 65536 个 /64，/64 前缀本身算 1 个，比位数更小的前缀也算 1 个。每次取下一个编号对应的子网，主机位随机：

- 用遍所有子网之前**不会重复**同一个子网
- 相邻两次不会落在挨着的子网（不是 `…:1::`、`…:2::` 这样顺序递增）
- 轮换序号由所有带 `-cidr-` 的连接和会话共享；服务启动时从随机位置开始，重启后不会重放同样的顺序
- 选中各前缀的概率和子网数量成正比：`/48 + /64` 在 `-cidr-64` 时几乎都落在 /48 里。不带 `-cidr-` 时是每个前缀各占一半
- 每个前缀最多参与轮换 2^40 个子网（HE 最大分配 /48，按 /64 才 2^16 个，碰不到）

常用值：`-cidr-64`（大多数网站按 /64 限流、封禁）、`-cidr-56`、`-cidr-48`（在多个前缀之间轮流）。

### 3.4 解析规则

- 第一个后缀之前的部分是基础用户名，用于认证
- 每个后缀的值一直到下一个后缀为止，所以**会话 ID 里不能包含** `-session-`、`-ttl-`、`-cidr-`、`-ip-`
- 紧跟在后缀名后面的部分属于值：`alice-session-ip-x` 的会话 ID 是 `ip-x`
- 同一个后缀出现两次、值不合法、`-ip-` 和其他后缀组合时，返回错误（HTTP 403，SOCKS5 应答 0x02）
- 先校验密码，再报告后缀错误；密码错误时一律按认证失败处理
- SOCKS5 用户名最长 255 字节（RFC 1929）

### 3.5 示例

```bash
P=change-me; S=203.0.113.10:24601
curl -x socks5h://alice:$P@$S https://api64.ipify.org                              # 随机
curl -x socks5h://alice-cidr-64:$P@$S https://api64.ipify.org                      # 每次换 /64
curl -x socks5h://alice-session-job1:$P@$S https://api64.ipify.org                 # 永久固定
curl -x socks5h://alice-session-job1-ttl-30-cidr-64:$P@$S https://api64.ipify.org  # 固定 30 分钟
curl -x http://alice-ip-2001-db8-abcd--1:$P@$S https://api64.ipify.org             # 指定地址
```

```python
# Python requests（需要 pip install requests[socks]）
proxies = {"https": "socks5h://alice-session-job1-ttl-30:change-me@203.0.113.10:24601"}
requests.get("https://api64.ipify.org", proxies=proxies)
```

---

## 4. 代理协议

### 4.1 同一个端口

`listen` 端口同时提供 SOCKS5 和 HTTP 代理：按连接的第一个字节区分，`0x05` 是 SOCKS5，其他都交给 HTTP 服务器处理（不是合法 HTTP 的数据会得到 `400`）。

### 4.2 认证

| `users` | SOCKS5 | HTTP |
|---|---|---|
| 非空 | 必须用用户名/密码认证（方法 0x02） | 必须带 `Proxy-Authorization: Basic …`，否则返回 `407` |
| 为空 | 接受无认证（方法 0x00）；客户端提供用户名/密码时任何密码都通过，用户名后缀照常生效 | 不要求认证，后缀照常生效 |

密码使用常数时间比较。

### 4.3 SOCKS5

| 项 | 支持情况 |
|---|---|
| 版本 | SOCKS5（RFC 1928、RFC 1929），不支持 SOCKS4 |
| 命令 | `CONNECT`（TCP）、`UDP ASSOCIATE`（UDP）；不支持 `BIND` |
| 目标地址 | 域名、IPv6；IPv4 目标会被拒绝 |
| UDP | 不支持分片；只接受建立关联的那个客户端 IP 发来的包；UDP 端口是服务器上的随机端口，iptables 规则只放行了 TCP，所以 UDP 只适合本机或同一内网使用 |

**推荐用 `socks5h://`**，让代理去解析域名。用 `socks5://` 的话，客户端会在本地解析，可能拿到 IPv4 地址被拒绝。

### 4.4 HTTP

| 请求 | 行为 |
|---|---|
| `CONNECT host:port`（https 等） | 建立 TCP 隧道，回复 `200 Connection established` |
| `GET http://…` 等 absolute-URI | 转发，每个请求新建上游连接，不添加 `X-Forwarded-For`，不暴露客户端 IP |
| 相对路径请求 | `400`，这是代理服务器而不是网站 |

### 4.5 超时与 DNS

| 项 | 值 |
|---|---|
| 连接建立后等待第一个字节 | 30 秒 |
| SOCKS5 握手（直到回复 CONNECT 结果） | 30 秒 |
| HTTP 请求头读取 | 30 秒 |
| 连接目标 | 30 秒 |
| 转发中的连接 | 无空闲超时，支持半关闭 |

域名用服务器的系统 DNS 解析，只查询 AAAA 记录，取第一个地址；DNS 请求本身走服务器的普通网络，不经过隧道。

---

## 5. 错误码速查

| 情况 | HTTP 代理 | SOCKS5 |
|---|---|---|
| 用户名或密码错误 | `407` | 认证应答状态 `0x01`，然后断开 |
| 客户端没有可接受的认证方法 | — | 方法应答 `0xFF` |
| 后缀不合法、`-ip-` 地址不在池内 | `403` | `0x02` 规则不允许 |
| 目标没有 IPv6 地址、DNS 解析失败 | `502` | `0x04` 主机不可达 |
| 目标拒绝连接 | `502` | `0x05` 连接被拒绝 |
| 其他连接失败、超时 | `502` | `0x01` 一般失败 |
| 不支持的命令（BIND） | — | `0x07` |
| 不支持的地址类型 | — | `0x08` |
| 相对路径请求、非法数据 | `400` | — |

---

## 6. scripts/check-prefix.sh

检查某个前缀是否已经被 HE 路由：用前缀内的地址经代理访问 `api64.ipify.org`，返回的出口地址等于该地址就算可用。适合 HE 新分配前缀后定时运行。

```bash
check-prefix.sh <前缀内的测试地址>    # 例如 check-prefix.sh 2001:db8:abcd::1
```

地址必须写成压缩的规范形式，并且所在前缀已经写进配置的 `prefixes`。退出码 0 表示可用（输出 `READY`），1 表示不可用（输出 `NOT-READY`），结果追加到日志。he-ipv6-pool 没在运行时会临时启动，检查完再停掉。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `BIN` | 脚本同目录的 `he-ipv6-pool` | 二进制路径 |
| `CONF` | 脚本同目录的 `config.json` | 临时启动时使用的配置 |
| `SOCKS` | `127.0.0.1:1080` | 代理地址 |
| `PROXY_USER` | `check` | 代理用户名（不含后缀） |
| `PROXY_PASS` | `x` | 代理密码 |
| `LOG` | 脚本同目录的 `check-prefix.log` | 日志文件 |

检查已安装的服务时：

```bash
SOCKS=127.0.0.1:<端口> PROXY_USER=<用户名> PROXY_PASS=<密码> ./check-prefix.sh 2001:db8:abcd::1
```

---

## 7. 发布（GitHub Actions）

`.github/workflows/release.yml` 先运行 `go vet` 和 `go test -race`，再用 Go 1.25 系列的最新补丁版交叉编译，最后创建 GitHub Release（发布说明根据提交记录自动生成）。两种触发方式：

| 方式 | 操作 | 说明 |
|---|---|---|
| 推送标签 | `git tag v0.1.0 && git push origin v0.1.0` | 在本地选定要发布的提交 |
| 手动触发 | Actions 页面选 **release** → **Run workflow** → 填版本号；或者 `gh workflow run release.yml -f version=v0.1.0` | 在所选分支的最新提交上自动创建标签，不需要本地操作 git |

**版本号规则**（[语义化版本](https://semver.org/lang/zh-CN/)）：

- 格式必须是 `vX.Y.Z`，否则工作流直接失败；手动触发时标签已存在也会失败
- 带后缀的版本（如 `v0.2.0-rc1`、`v1.0.0-beta.2`）发布为**预发布版本**，不会成为 `releases/latest`，README 里的一键安装命令也不会下载到它
- 只修 bug 加 Z，加功能加 Y，有不兼容的改动（配置字段、用户名后缀的含义变化等）加 X

| 产物 | 平台 |
|---|---|
| `he-ipv6-pool-linux-amd64` / `-arm64` / `-arm` / `-386` / `-riscv64` | Linux |
| `he-ipv6-pool-darwin-amd64` / `-arm64` | macOS（只能前台运行，没有管理命令；还没有在真实隧道上验证过） |
| `SHA256SUMS` | 校验和 |

同时会构建 linux/amd64 和 linux/arm64 的 Docker 镜像推送到 `ghcr.io/x6nux/he-ipv6-pool`，镜像标签见 [1.8](#18-docker)；镜像任务在测试和版本号校验通过后才会运行。

二进制是静态编译的（`CGO_ENABLED=0`）。`he-ipv6-pool -version` 显示版本，`install` 和 `config` 打印的信息里也有版本：

| 构建方式 | `-version` 显示 |
|---|---|
| Release 下载 | 标签名，例如 `v0.1.0` |
| `go install github.com/x6nux/he-ipv6-pool@v0.1.0` | 模块版本 `v0.1.0` |
| 从源码 `go build` | 提交时间和提交号，例如 `v0.0.0-20261022085723-8dd9972282ed`，有未提交的改动时带 `+dirty` |

升级已安装的服务：下载新版本后执行 `sudo ./he-ipv6-pool install`，配置会保留。

---

## 8. 限制

- 只出 IPv6：目标只有 IPv4 时直接拒绝，不会回退到直连
- 不支持 SOCKS5 `BIND` 和 UDP 分片；UDP 只适合本机或同一内网使用
- SOCKS5 `UDP ASSOCIATE` 回复的 `BND.ADDR` 是本机地址，处在 NAT 后面时远程客户端会收到内网 IP
- 不更新 HE 的动态 IPv4（可以用 cron 调用 tunnelbroker 的 update URL）
- 管理命令只支持 Linux + systemd；不支持 Windows
- 不要同时启用内核的 HE 隧道（`iface he-ipv6 inet6 v4tunnel`）：内核会处理同样的协议 41 包，可能回发 ICMP 不可达干扰连接。先 `ifdown he-ipv6`，仍有问题再 `modprobe -r sit`
