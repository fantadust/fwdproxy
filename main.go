// fwdproxy —— 极简 HTTP/HTTPS 正向代理，供内网机器访问外部 API。
//
// 特性：CONNECT 隧道（HTTPS）+ 明文 HTTP 转发、Proxy Basic 认证、目标域名白名单、
// 访问日志（自动轮转）、并发连接数上限。只依赖 Go 标准库。
//
// 部署布局（二进制放哪，配置和日志就在哪，无需手工建目录）：
//
//	/data/apps/fwdproxy/fwdproxy        二进制
//	/data/apps/fwdproxy/fwdproxy.conf   配置文件（自动读取）
//	/data/apps/fwdproxy/log/            日志目录（自动创建）
package main

import (
	"bufio"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	configPath = flag.String("config", "", "配置文件路径；留空则读二进制同目录下的 fwdproxy.conf（不存在则忽略）")

	addr     = flag.String("addr", ":8443", "监听地址，例：:8443 或 0.0.0.0:8443")
	user     = flag.String("user", "", "Basic 认证用户名（单用户简写，等价于一条 auth）")
	pass     = flag.String("pass", "", "Basic 认证密码（单用户简写）")
	noAuth   = flag.Bool("no-auth", false, "显式关闭认证（危险：将成为开放代理，仅限本机调试）")
	allowRaw = flag.String("allow", "", "目标域名白名单，逗号分隔；留空表示允许全部目标")
	dialTO   = flag.Duration("dial-timeout", 15*time.Second, "连接上游超时")

	tlsCert = flag.String("tls-cert", "", "TLS 证书路径；配上后客户端到代理这一段走 HTTPS（客户端需用 https:// 代理 URL）")
	tlsKey  = flag.String("tls-key", "", "TLS 私钥路径，与 -tls-cert 必须同时提供")

	maxConns   = flag.Int("max-conns", 512, "最大并发连接数，超出的连接在内核 backlog 排队；0 表示不限制")
	tunnelIdle = flag.Duration("tunnel-idle-timeout", 15*time.Minute,
		"CONNECT 隧道空闲超时，用于回收死连接。务必大于上游最慢的响应耗时（生图类请求可达 600s）；0 表示禁用")

	logFile   = flag.String("log-file", "", "日志文件路径；留空则写二进制同目录下的 log/fwdproxy.log；填 stdout 则输出到标准输出")
	logMaxMB  = flag.Int("log-max-mb", 64, "单个日志文件大小上限（MB），超过即轮转；0 表示不轮转")
	logKeep   = flag.Int("log-keep", 7, "轮转后保留的历史日志份数")
	accessLog = flag.Bool("access-log", true, "记录每条请求的访问日志（只记目标 host，不记 path/query，避免泄漏 URL 里的 token）")
)

// multiFlag 收集可重复出现的配置项：配置文件里写多行 auth 会依次累加而非覆盖。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// authList 形如 name:password，可重复出现，用于区分多个调用方。
var authList multiFlag

func init() {
	flag.Var(&authList, "auth", "认证凭据，格式 name:password；可重复出现以配置多个用户")
}

// cred 是一条已展开的凭据；blob 即 Basic 认证解码后应当匹配的 "name:password"。
type cred struct {
	name string
	blob []byte
}

var (
	creds []cred
	allow []string
	// 当前活跃连接数，仅用于日志观测。
	activeConns atomic.Int64
	// 请求序号，用于把 start / done 两条日志配对。
	reqSeq atomic.Int64
)

// reqCtx 汇总一次请求的日志字段。
type reqCtx struct {
	id     int64
	user   string
	client string
	target string
	start  time.Time
	conns  int64
}

func newReqCtx(r *http.Request, who, target string) *reqCtx {
	return &reqCtx{
		id:     reqSeq.Add(1),
		user:   who,
		client: clientIP(r),
		target: target,
		start:  time.Now(),
		conns:  activeConns.Load(),
	}
}

// logStart 在请求刚进来时就落一条。长请求（生图可达几百秒）如果只在结束时
// 打日志，这期间从日志上完全看不出有请求正在跑。id 用来和 done 那条配对。
func (c *reqCtx) logStart(method string) {
	logAccess("start %s %s id=%d user=%s client=%s conns=%d",
		method, c.target, c.id, c.user, c.client, c.conns)
}

// 出站 Transport：显式 Proxy=nil，避免本机 env 里的 *_PROXY 造成套娃。
var transport = &http.Transport{
	Proxy:        nil,
	DialContext:  (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns: 100,
	// 默认 MaxIdleConnsPerHost 只有 2，本代理的流量高度集中在少数上游 host，
	// 太小会导致并发时反复三次握手 + TLS 握手。
	MaxIdleConnsPerHost: 64,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 15 * time.Second,
}

// exeDir 返回二进制真实所在目录（解析软链），配置与日志都相对它定位，
// 这样 systemd 把 WorkingDirectory 设成 / 也不影响。
func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Dir(p)
}

// ---------- 配置文件 ----------

// loadConfig 解析 "key = value" 形式的配置文件（整行以 # 开头为注释），把其中
// 的项应用到同名 flag 上。命令行显式给出的 flag 优先，配置文件不覆盖它。
// key 里的下划线等价于 flag 名中的连字符（max_conns == max-conns）。
func loadConfig(path string, explicit map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d 不是 key = value 格式：%q", path, ln, line)
		}
		k = strings.ReplaceAll(strings.TrimSpace(k), "_", "-")
		v = unquote(strings.TrimSpace(v))
		if flag.Lookup(k) == nil {
			return fmt.Errorf("%s:%d 未知配置项 %q", path, ln, k)
		}
		if explicit[k] {
			continue
		}
		if err := flag.Set(k, v); err != nil {
			return fmt.Errorf("%s:%d 配置项 %s 取值无效：%v", path, ln, k, err)
		}
	}
	return sc.Err()
}

// unquote 去掉成对的首尾引号：配置文件不是 shell，写了引号会变成值的一部分，
// 密码里混进引号是排查起来最费时的那类问题，这里直接消化掉。
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// ---------- 日志 ----------

// rotatingFile 是按大小轮转的日志 writer：超过 maxBytes 就把当前文件改名为
// .1，历史依次后推，最多留 keep 份。收到 SIGHUP 会重开文件，兼容外部 logrotate。
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func newRotatingFile(path string, maxBytes int64, keep int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.reopen(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) reopen() error {
	if r.f != nil {
		r.f.Close()
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) Reopen() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reopen()
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maxBytes > 0 && r.size+int64(len(p)) > r.maxBytes && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) rotate() {
	r.f.Close()
	if r.keep <= 0 {
		os.Remove(r.path)
		r.reopen()
		return
	}
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	os.Rename(r.path, r.path+".1")
	r.reopen()
}

func logAccess(format string, v ...any) {
	if *accessLog {
		log.Printf(format, v...)
	}
}

// fatalf 同时写日志和 stderr：日志已切到文件后，systemctl status 仍能看到失败原因。
func fatalf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	log.Print(msg)
	if *logFile != "stdout" {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(1)
}

// ---------- 连接数限制 ----------

// limitListener 给 Accept 出来的连接加并发上限和活跃计数。达到上限时不是拒绝，
// 而是暂停 Accept，让新连接留在内核 backlog 里排队，客户端表现为稍慢而非报错。
type limitListener struct {
	net.Listener
	sem chan struct{} // nil 表示不限流
}

func (l *limitListener) Accept() (net.Conn, error) {
	if l.sem != nil {
		l.sem <- struct{}{}
	}
	c, err := l.Listener.Accept()
	if err != nil {
		if l.sem != nil {
			<-l.sem
		}
		return nil, err
	}
	activeConns.Add(1)
	return &trackedConn{Conn: c, sem: l.sem}, nil
}

// trackedConn 在 Close 时释放配额。CONNECT 隧道 hijack 后连接由我们自己关闭，
// 计数依然准确。代价是它遮住了底层 *net.TCPConn，io.Copy 走不到 splice 零拷贝
// 快路径——本场景单请求也就几 MB，这点拷贝开销远小于连接数失控的风险。
type trackedConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		activeConns.Add(-1)
		if c.sem != nil {
			<-c.sem
		}
	})
	return err
}

// ---------- 代理逻辑 ----------

func hostAllowed(hostport string) bool {
	if len(allow) == 0 {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// authUser 校验 Proxy-Authorization，返回命中的用户名。
// 刻意遍历完全部凭据而不提前 break：提前返回会让"用户名不存在"和"密码错误"
// 的耗时出现差异，给爆破留下时序侧信道。
func authUser(r *http.Request) (string, bool) {
	if *noAuth {
		return "-", true
	}
	const prefix = "Basic "
	v := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(v, prefix) {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v[len(prefix):]))
	if err != nil {
		return "", false
	}
	matched := ""
	for _, c := range creds {
		if subtle.ConstantTimeCompare(raw, c.blob) == 1 {
			matched = c.name
		}
	}
	return matched, matched != ""
}

func handle(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.Host
	}

	who, ok := authUser(r)
	if !ok {
		// 认证失败不受 -access-log 控制：这是安全事件，用来发现扫描和爆破。
		log.Printf("认证失败 client=%s method=%s target=%s", clientIP(r), r.Method, target)
		w.Header().Set("Proxy-Authenticate", `Basic realm="fwdproxy"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}

	if !hostAllowed(target) {
		log.Printf("拒绝目标 %s user=%s client=%s", target, who, clientIP(r))
		http.Error(w, "target not allowed", http.StatusForbidden)
		return
	}

	c := newReqCtx(r, who, target)
	if r.Method == http.MethodConnect {
		tunnel(w, r, c)
		return
	}
	forward(w, r, c)
}

// tunnel 处理 CONNECT：与上游建 TCP，然后 hijack 客户端连接双向拷贝。
func tunnel(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	c.target = withDefaultPort(r.Host, "443")
	c.logStart("CONNECT")

	upstream, err := net.DialTimeout("tcp", c.target, *dialTO)
	if err != nil {
		log.Printf("done  CONNECT %s id=%d user=%s client=%s status=502 dur=%s err=%v",
			c.target, c.id, c.user, c.client, dur(c.start), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	var up int64
	done := make(chan struct{})
	// 上行用 brw.Reader 而非 client：hijack 时缓冲区里可能已经读进了客户端数据。
	go func() {
		defer close(done)
		up, _ = copyIdle(upstream, brw, client)
		if c, ok := upstream.(*net.TCPConn); ok {
			c.CloseWrite()
		}
	}()
	down, _ := copyIdle(client, upstream, upstream)

	// 先关连接逼上行 goroutine 退出，再等它结束，这样读 up 既无 data race 又准确。
	client.Close()
	upstream.Close()
	<-done

	logAccess("done  CONNECT %s id=%d user=%s client=%s status=200 up=%d down=%d dur=%s",
		c.target, c.id, c.user, c.client, up, down, dur(c.start))
}

// copyIdle 把 src 拷到 dst；tunnelIdle > 0 时每次读前重设读超时，回收那些 TCP
// 层还没察觉的死连接，免得它们一直占着并发配额。
// 注意：超时值必须大于上游最慢的响应耗时，否则会把"正常的长时间等待"误判成空闲
// 而掐断连接——生图类请求可以几百秒没有任何字节流动。
func copyIdle(dst io.Writer, src io.Reader, srcConn net.Conn) (int64, error) {
	if *tunnelIdle <= 0 {
		return io.Copy(dst, src)
	}
	buf := make([]byte, 32*1024)
	var total int64
	for {
		if err := srcConn.SetReadDeadline(time.Now().Add(*tunnelIdle)); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}

// forward 处理明文 HTTP 的绝对 URI 请求。
func forward(w http.ResponseWriter, r *http.Request, c *reqCtx) {
	if !r.URL.IsAbs() {
		http.Error(w, "absolute URI required", http.StatusBadRequest)
		return
	}
	c.logStart(r.Method)

	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Authorization")
	out.Header.Del("Proxy-Connection")

	resp, err := transport.RoundTrip(out)
	if err != nil {
		log.Printf("done  %s %s id=%d user=%s client=%s status=502 dur=%s err=%v",
			r.Method, c.target, c.id, c.user, c.client, dur(c.start), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)

	logAccess("done  %s %s id=%d user=%s client=%s status=%d down=%d dur=%s",
		r.Method, c.target, c.id, c.user, c.client, resp.StatusCode, n, dur(c.start))
}

// ---------- 小工具 ----------

func withDefaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(hostport, port)
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func dur(start time.Time) time.Duration {
	return time.Since(start).Round(time.Millisecond)
}

// ---------- 启动 ----------

func main() {
	flag.Parse()

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// 未显式指定 -config 时，找二进制同目录的 fwdproxy.conf；找不到就用默认值继续。
	cfg, required := *configPath, true
	if cfg == "" {
		cfg, required = filepath.Join(exeDir(), "fwdproxy.conf"), false
	}
	if err := loadConfig(cfg, explicit); err != nil {
		if required || !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "读取配置失败：%v\n", err)
			os.Exit(1)
		}
		cfg = ""
	}

	// 日志：默认写二进制同目录的 log/fwdproxy.log，目录自动创建。
	if *logFile == "" {
		*logFile = filepath.Join(exeDir(), "log", "fwdproxy.log")
	}
	if *logFile != "stdout" {
		rf, err := newRotatingFile(*logFile, int64(*logMaxMB)<<20, *logKeep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "打开日志文件 %s 失败：%v\n", *logFile, err)
			os.Exit(1)
		}
		log.SetOutput(rf)
		// SIGHUP 重开日志文件，兼容外部 logrotate。
		go func() {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, syscall.SIGHUP)
			for range ch {
				rf.Reopen()
			}
		}()
	}

	for _, a := range strings.Split(*allowRaw, ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			allow = append(allow, a)
		}
	}

	// 凭据：user/pass 是单用户简写，auth 可重复出现配置多个用户。
	if *user != "" || *pass != "" {
		if *user == "" || *pass == "" {
			fatalf("user 和 pass 必须同时配置")
		}
		creds = append(creds, cred{name: *user, blob: []byte(*user + ":" + *pass)})
	}
	seenName := map[string]bool{}
	for _, a := range authList {
		name, pw, ok := strings.Cut(a, ":")
		if !ok || name == "" || pw == "" {
			fatalf("auth 格式应为 name:password，实际为 %q", a)
		}
		if seenName[name] {
			fatalf("auth 用户名重复：%q", name)
		}
		seenName[name] = true
		creds = append(creds, cred{name: name, blob: []byte(a)})
	}
	// fail-closed：配置文件缺失或键名写错时宁可起不来，也不能静默变成开放代理。
	if !*noAuth && len(creds) == 0 {
		fatalf("未配置认证凭据：请在配置文件里设置 user/pass 或 auth；确需关闭认证请显式加 -no-auth")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fatalf("监听 %s 失败：%v", *addr, err)
	}
	// 可选：客户端到代理这一段也走 TLS。被代理的 HTTPS 流量本身是端到端加密的，
	// 与此无关；这里保护的是 Proxy-Authorization 头不在公网上明文传输。
	scheme := "http"
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			fatalf("tls-cert 与 tls-key 必须同时配置")
		}
		pair, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fatalf("加载证书失败：%v", err)
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		})
		scheme = "https"
	}
	var sem chan struct{}
	if *maxConns > 0 {
		sem = make(chan struct{}, *maxConns)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(handle),
		// 只限制读 header：生图/长响应可能跑几分钟，设 Read/WriteTimeout 会中途掐断。
		ReadHeaderTimeout: 20 * time.Second,
		ErrorLog:          log.Default(),
	}

	if cfg != "" {
		log.Printf("配置文件 %s", cfg)
	}
	log.Printf("fwdproxy 监听 %s://%s 用户数=%d 白名单=%v 最大连接=%d 隧道空闲超时=%s",
		scheme, *addr, len(creds), allowDesc(), *maxConns, *tunnelIdle)
	if *noAuth {
		log.Printf("警告：已用 -no-auth 关闭认证，任何能连到本端口的人都可使用本代理")
	}
	if len(allow) == 0 {
		log.Printf("提醒：未设置 allow 白名单，允许访问任意目标；请确保防火墙只放行可信来源 IP")
	}
	log.Fatal(srv.Serve(&limitListener{Listener: ln, sem: sem}))
}

func allowDesc() string {
	if len(allow) == 0 {
		return "全部"
	}
	return strings.Join(allow, ",")
}
