# fwdproxy

English | [简体中文](README.md)

A minimal HTTP/HTTPS forward proxy for machines that cannot reach the public
internet directly. Standard library only, static binary, no runtime dependencies.

## Features

- CONNECT tunneling (HTTPS) + plain HTTP forwarding
- Proxy Basic authentication, multiple users, per-user access logs
- Destination host allowlist (unrestricted by default)
- Automatic log rotation, reopen on SIGHUP
- Concurrent connection cap, tunnel idle timeout
- Optional TLS: encrypts the *client-to-proxy* hop

## Layout

Config and logs live next to the binary — no directories to create by hand:

```
/data/apps/fwdproxy/
├── fwdproxy          the binary
├── fwdproxy.conf     config file (auto-loaded from the same directory)
└── log/              created automatically
    ├── fwdproxy.log
    └── fwdproxy.log.1 ...   (rotated, 64MB × 7 by default)
```

## Deployment

Download the binary from the [Releases](../../releases) page (`fwdproxy-linux-amd64`,
x86-64). For other architectures, build from source — see [Building](#building).

The architecture suffix in the filename is deliberate: because the uploaded file has
a different name than the running `fwdproxy`, `rz`/`scp` will not fail with `ETXTBSY`
("cannot overwrite a running binary"). Rename it after it lands.

```bash
# 1. Upload the binary and the config (scp or rz into any writable dir, e.g. /tmp)
sudo mkdir -p /data/apps/fwdproxy
sudo mv fwdproxy-linux-amd64 /data/apps/fwdproxy/fwdproxy
sudo chmod +x /data/apps/fwdproxy/fwdproxy
sudo cp fwdproxy.conf.example /data/apps/fwdproxy/fwdproxy.conf
sudo vi /data/apps/fwdproxy/fwdproxy.conf     # at minimum, change user/pass

# 2. Create a service account and tighten permissions
sudo useradd -r -s /usr/sbin/nologin fwdproxy
sudo chown -R fwdproxy:fwdproxy /data/apps/fwdproxy
sudo chown root:fwdproxy /data/apps/fwdproxy/fwdproxy.conf
sudo chmod 640 /data/apps/fwdproxy/fwdproxy.conf   # holds the password, keep it tight

# 3. Install the systemd unit
sudo cp fwdproxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fwdproxy
sudo systemctl status fwdproxy
```

`systemctl reload fwdproxy` sends SIGHUP to reopen the log file, so an external
logrotate can be used.

## Upgrading an existing deployment

A running binary cannot be overwritten in place. Rename first, then replace (`mv` on a
running file is allowed — it only changes the directory entry; the process keeps using
the old inode):

```bash
# after uploading fwdproxy-linux-amd64 to /tmp
sudo systemctl stop fwdproxy
sudo mv /data/apps/fwdproxy/fwdproxy /data/apps/fwdproxy/fwdproxy.bak   # keep it for rollback
sudo mv /tmp/fwdproxy-linux-amd64 /data/apps/fwdproxy/fwdproxy
sudo chmod +x /data/apps/fwdproxy/fwdproxy
sudo chown fwdproxy:fwdproxy /data/apps/fwdproxy/fwdproxy
sudo systemctl start fwdproxy && sudo systemctl status fwdproxy
```

To confirm the new build is live: it logs two lines per request. Run
`tail -f log/fwdproxy.log`, send one request, and look for `start CONNECT ... id=1`.
A single `CONNECT ...` line means the old binary is still running. Delete
`fwdproxy.bak` once you are satisfied.

## Firewall: mandatory, do not skip

An HTTP proxy exposed on the public internet is usually found by scanners and abused as
an open proxy within hours. Allow only the client machine's IP:

```bash
# firewalld
sudo firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=CLIENT_IP/32 port port=8443 protocol=tcp accept'
sudo firewall-cmd --reload

# or iptables
sudo iptables -A INPUT -p tcp --dport 8443 -s CLIENT_IP -j ACCEPT
sudo iptables -A INPUT -p tcp --dport 8443 -j DROP
```

## Client configuration

```bash
# Use http:// when the proxy has no TLS; use https:// for both when TLS is enabled.
# This scheme describes the connection *to the proxy*, not the traffic being proxied.
HTTP_PROXY=http://user:password@PROXY_IP:8443
HTTPS_PROXY=http://user:password@PROXY_IP:8443
NO_PROXY=localhost,127.0.0.1,::1,.aliyuncs.com,.volces.com,internal.example.com
```

**`NO_PROXY` is not optional.** Without it, a global `HTTPS_PROXY` also diverts endpoints
that were already reachable directly — an extra hop at best, a broken connection at
worst. httpx / requests default to `trust_env=True` and pick these variables up
indiscriminately.

If the password contains `@`, `:`, `/` and similar characters, URL-encode it inside the
proxy URL.

Verified working with requests 2.32 and httpx 0.28 (the transport under most Python SDKs),
for `https://` proxies, both via explicit arguments and via the `HTTPS_PROXY`
environment variable — callers need no code changes.

Verify:

```bash
curl -x "http://user:password@PROXY_IP:8443" https://api.example.com/v1/models -H "Authorization: Bearer $API_KEY"
```

Note that the upstream API sees the **proxy machine's** egress IP, not the client's. If
the API key is IP-restricted, allowlist the proxy machine.

## Logs

Two lines per request — one `start` on arrival, one `done` on completion, paired by `id=`:

```
start CONNECT api.example.com:443 id=42 user=ai-img client=1.2.3.4 conns=3
done  CONNECT api.example.com:443 id=42 user=ai-img client=1.2.3.4 status=200 up=585 down=4823 dur=218ms
认证失败 client=5.6.7.8 method=CONNECT target=api.example.com:443
拒绝目标 github.com:443 user=ai-img client=1.2.3.4
```

Why `start` exists: requests such as image generation can take hundreds of seconds. If
only the completion were logged, nothing in the log would show that a request is in
flight. Concurrent requests interleave the two lines — pair them by `id=`.

`up`/`down` are the tunnel's byte counts in each direction; `conns` is the number of
active connections when the request arrived. Auth failures and rejected destinations are
always recorded regardless of the `access-log` switch — that is how you spot scanning and
brute-force attempts. Only the destination host is logged, never path or query, so tokens
embedded in URLs never hit disk.

```bash
tail -f log/fwdproxy.log | grep start          # only requests currently arriving
grep 'id=42 ' log/fwdproxy.log                 # the full lifecycle of one request
grep -c '^.\{20\}start' log/fwdproxy.log       # request count
```

## Configuration

All options are documented in `fwdproxy.conf.example`. Command-line flags take precedence
over the config file; `./fwdproxy -h` lists them.

A few easy ones to get wrong:

- `tunnel-idle-timeout` must exceed the slowest upstream response. Image-generation
  requests can go hundreds of seconds without a single byte flowing; set it too low and a
  healthy connection gets killed as dead.
- An empty `allow` means any destination is permitted — security then rests entirely on
  authentication plus the firewall.
- With no credential configured and no `-no-auth` flag, the program refuses to start
  rather than silently becoming an open proxy.

## Building

No go.mod required — build straight from the bare directory:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o fwdproxy-linux-amd64 main.go
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o fwdproxy-linux-arm64 main.go
```

## License

[MIT](LICENSE)
