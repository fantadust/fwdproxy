# fwdproxy

简体中文 | [English](README.en.md)

极简 HTTP/HTTPS 正向代理，给访问不了外网的机器用。只依赖 Go 标准库，静态二进制无运行时依赖。

## 能力

- CONNECT 隧道（HTTPS）+ 明文 HTTP 转发
- Proxy Basic 认证，支持多用户，访问日志按用户区分
- 目标域名白名单（默认不限制）
- 访问日志自动轮转，支持 SIGHUP 重开
- 并发连接数上限、隧道空闲超时
- 可选 TLS：给"客户端到代理"这一段加密

## 目录布局

二进制放哪，配置和日志就在哪，不需要手工建目录：

```
/data/apps/fwdproxy/
├── fwdproxy          二进制
├── fwdproxy.conf     配置文件（同目录自动读取）
└── log/              自动创建
    ├── fwdproxy.log
    └── fwdproxy.log.1 ... （自动轮转，默认 64MB × 7 份）
```

## 部署

从 [Releases](../../releases) 下载 `fwdproxy-linux-amd64`（x86-64）。其他架构（如 arm64）
自行编译，见文末「重新编译」。

带架构后缀上传是有意的：文件名和运行中的 `fwdproxy` 不同名，`rz`/`scp` 就不会因为
「正在执行的二进制不可覆盖」（`ETXTBSY`）而失败，落地后再改名即可。

```bash
# 1. 上传二进制与配置（scp 或 rz 到任意可写目录，如 /tmp）
sudo mkdir -p /data/apps/fwdproxy
sudo mv fwdproxy-linux-amd64 /data/apps/fwdproxy/fwdproxy
sudo chmod +x /data/apps/fwdproxy/fwdproxy
sudo cp fwdproxy.conf.example /data/apps/fwdproxy/fwdproxy.conf
sudo vi /data/apps/fwdproxy/fwdproxy.conf     # 至少改掉 user/pass

# 2. 建服务账号并收紧权限
sudo useradd -r -s /usr/sbin/nologin fwdproxy
sudo chown -R fwdproxy:fwdproxy /data/apps/fwdproxy
sudo chown root:fwdproxy /data/apps/fwdproxy/fwdproxy.conf
sudo chmod 640 /data/apps/fwdproxy/fwdproxy.conf   # 密码在里面，别放宽

# 3. 装 systemd 服务
sudo cp fwdproxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fwdproxy
sudo systemctl status fwdproxy
```

`systemctl reload fwdproxy` 会发 SIGHUP 重开日志文件，供外部 logrotate 使用。

## 升级已有部署

运行中的二进制不能被直接覆盖，先改名再替换（`mv` 对运行中的文件是允许的，
只改目录项，进程继续用旧 inode）：

```bash
# 上传 fwdproxy-linux-amd64 到 /tmp 后
sudo systemctl stop fwdproxy
sudo mv /data/apps/fwdproxy/fwdproxy /data/apps/fwdproxy/fwdproxy.bak   # 留着好回滚
sudo mv /tmp/fwdproxy-linux-amd64 /data/apps/fwdproxy/fwdproxy
sudo chmod +x /data/apps/fwdproxy/fwdproxy
sudo chown fwdproxy:fwdproxy /data/apps/fwdproxy/fwdproxy
sudo systemctl start fwdproxy && sudo systemctl status fwdproxy
```

确认新版生效：新版每个请求打两条日志，`tail -f log/fwdproxy.log` 后发一个请求，
看到 `start CONNECT ... id=1` 即为新版；只有一条 `CONNECT ...` 说明还是旧的。
确认无误后再删 `fwdproxy.bak`。

## 防火墙：必做，别省

公网上裸奔的 HTTP 代理通常几小时内就会被扫到并当成开放代理滥用。只放行部署机 IP：

```bash
# firewalld
sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=部署机IP/32 port port=8443 protocol=tcp accept'
sudo firewall-cmd --reload

# 或 iptables
sudo iptables -A INPUT -p tcp --dport 8443 -s 部署机IP -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 8443 -j DROP
```

## 客户端（部署机）配置

```bash
# 代理未启用 TLS 时用 http://；启用了 TLS 则两个都写 https://
# 这个 scheme 指的是"到代理的连接"，不是"被代理的流量"
HTTP_PROXY=http://用户:密码@代理机IP:8443
HTTPS_PROXY=http://用户:密码@代理机IP:8443
NO_PROXY=localhost,127.0.0.1,::1,.aliyuncs.com,.volces.com,dashscope.aliyuncs.com,ark.cn-beijing.volces.com,内网域名
```

**`NO_PROXY` 必须配。** 否则设了全局 `HTTPS_PROXY` 之后，本来直连就通的国内接口（通义千问、豆包等）也会被绕去代理，白白多一跳甚至连不通——底层 httpx / requests 默认 `trust_env=True`，会无差别读取这些环境变量。

密码里有 `@` `:` `/` 等字符时要按 URL 编码写进代理 URL。

已实测 requests 2.32 / httpx 0.28（含 OpenAI SDK 底层）对 `https://` 代理支持完好，显式传参和读 `HTTPS_PROXY` 环境变量都可用，调用方无需改代码。

验证：

```bash
curl -x "http://用户:密码@代理机IP:8443" https://api.openai.com/v1/models -H "Authorization: Bearer $OPENAI_API_KEY"
```

注意 OpenAI 看到的来源 IP 是**代理机**的出口 IP，不是部署机的。API key 若绑了 IP 白名单，要绑代理机 IP。

## 日志

每个请求落两条日志，进来时一条 `start`，结束时一条 `done`，用 `id=` 配对：

```
start CONNECT api.openai.com:443 id=42 user=ai-img client=1.2.3.4 conns=3
done  CONNECT api.openai.com:443 id=42 user=ai-img client=1.2.3.4 status=200 up=585 down=4823 dur=218ms
认证失败 client=5.6.7.8 method=CONNECT target=api.openai.com:443
拒绝目标 github.com:443 user=ai-img client=1.2.3.4
```

之所以要 `start`：生图这类请求可能几百秒才返回，只在结束时打日志的话，这期间从日志上完全看不出有请求在跑。并发时两条日志会互相穿插，靠 `id=` 配对。

`up`/`down` 是隧道双向字节数，`conns` 是请求进入时的活跃连接数。认证失败和拒绝目标不受 `access-log` 开关控制，始终记录——用来发现扫描和爆破。日志只记目标 host，不记 path/query，避免 URL 里的 token 落盘。

```bash
tail -f log/fwdproxy.log | grep start          # 只看正在进来的请求
grep 'id=42 ' log/fwdproxy.log                 # 看某个请求的完整生命周期
grep -c '^.\{20\}start' log/fwdproxy.log      # 统计请求数
```

## 配置项

全部配置见 `fwdproxy.conf.example`，命令行参数优先级高于配置文件，`./fwdproxy -h` 可查。

几个容易踩的：

- `tunnel-idle-timeout` 必须大于上游最慢的响应耗时。生图类请求可能几百秒没有任何字节流动，设小了会被当成死连接掐断。
- `allow` 留空表示允许访问任意目标，此时安全完全依赖认证 + 防火墙。
- 一个凭据都没配又没加 `-no-auth` 时，程序拒绝启动，不会静默变成开放代理。

## 重新编译

无需 go.mod，裸目录直接编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o fwdproxy-linux-amd64 main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o fwdproxy-linux-arm64 main.go
```
