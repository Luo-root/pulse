package builtins

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Luo-root/pulse/toolset"
)

// lookupIPAddr 默认走系统解析；测试可替换以覆盖 DNS 路径（rebinding / mapped IPv6）。
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func parseHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("builtins: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("builtins: bad url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("builtins: url scheme %q rejected (http/https only)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("builtins: url host is required")
	}
	return u, nil
}

func blockedMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// 阿里云 metadata
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	return false
}

// hostClassOf 给出 host 的**字面**类别，供 HITL 卡片（NetworkChange.HostClass）
// 使用，取值见 toolset.HostClass* 常量。
//
// 这里**不做 DNS**：PreviewFn 在**人批之前**跑，解析域名等于让未获准的动作先
// 产生外部流量（DNS 也可当信道）。所以只有能按字面判定的才给结论——IP 字面量
// 走与连接期同一套判据（blockedMetadataIP / IsPrivate / IsLoopback），本地名
// 记 private，其余域名一律 unknown，让「按类别放行」的策略落到问人的分支。
// 真实落点的拦截在连接期按解析结果做（checkHost / guardedDial）。
func hostClassOf(host string) string {
	h := strings.ToLower(splitHost(host))
	if h == "" {
		return toolset.HostClassUnknown
	}
	if ip := net.ParseIP(h); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		switch {
		case blockedMetadataIP(ip):
			return toolset.HostClassMetadata
		case ip.IsPrivate() || ip.IsLoopback():
			return toolset.HostClassPrivate
		default:
			return toolset.HostClassPublic
		}
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return toolset.HostClassPrivate
	}
	return toolset.HostClassUnknown
}

// splitHost 把 host[:port] / [v6]:port 归一成裸主机名。
//
// 预览侧（hostClassOf）与连接侧（checkHost）共用这一份：两处各写一遍
// 迟早分叉——本轮就是一处多一个 ToLower、一处走 DNS 而不特判 localhost
// （#215 review 建议）。**不在这里做大小写归一**：连接侧保持原样交给
// net.LookupIP / net.ParseIP，预览侧自己 ToLower。
func splitHost(host string) string {
	h := host
	if h2, _, err := net.SplitHostPort(host); err == nil {
		h = h2
	}
	return strings.Trim(h, "[]")
}

func checkHost(ctx context.Context, host string, blockPrivate bool) error {
	h := splitHost(host)
	if ip := net.ParseIP(h); ip != nil {
		return checkIP(ip, blockPrivate)
	}
	ips, err := lookupIPAddr(ctx, h)
	if err != nil {
		return fmt.Errorf("builtins: resolve %s: %w", h, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("builtins: resolve %s: no addresses", h)
	}
	for _, ipa := range ips {
		if err := checkIP(ipa.IP, blockPrivate); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP, blockPrivate bool) error {
	if ip == nil {
		return fmt.Errorf("builtins: nil address")
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if blockedMetadataIP(ip) {
		return fmt.Errorf("builtins: refusing metadata/link-local address %s", ip)
	}
	if blockPrivate && (ip.IsPrivate() || ip.IsLoopback()) {
		return fmt.Errorf("builtins: refusing private/loopback address %s", ip)
	}
	return nil
}

// guardedClient 浅拷贝 c，在 Dial 时对实际解析结果跑 checkIP（覆盖 redirect 与 DNS rebinding）。
// 不修改调用方的 Client。c == nil 时用 DefaultHTTPTimeout。
func guardedClient(c *http.Client, blockPrivate bool) *http.Client {
	out := &http.Client{Timeout: DefaultHTTPTimeout}
	if c != nil {
		cp := *c
		out = &cp
		if out.Timeout == 0 {
			out.Timeout = DefaultHTTPTimeout
		}
	}

	var parentDial func(context.Context, string, string) (net.Conn, error)
	switch t := out.Transport.(type) {
	case *http.Transport:
		nt := t.Clone()
		parentDial = nt.DialContext
		nt.DialContext = guardedDial(parentDial, blockPrivate)
		out.Transport = nt
	case nil:
		nt := http.DefaultTransport.(*http.Transport).Clone()
		parentDial = nt.DialContext
		nt.DialContext = guardedDial(parentDial, blockPrivate)
		out.Transport = nt
	default:
		// 未知 RoundTripper：至少在每一跳 RoundTrip 前再检一次 Host（拦 redirect）。
		out.Transport = hostGuardRT{base: t, blockPrivate: blockPrivate}
	}

	origCR := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := checkHost(req.Context(), req.URL.Host, blockPrivate); err != nil {
			return err
		}
		if origCR != nil {
			return origCR(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return out
}

type hostGuardRT struct {
	base         http.RoundTripper
	blockPrivate bool
}

func (g hostGuardRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkHost(req.Context(), req.URL.Host, g.blockPrivate); err != nil {
		return nil, err
	}
	return g.base.RoundTrip(req)
}

func guardedDial(parent func(context.Context, string, string) (net.Conn, error), blockPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	if parent == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		parent = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); ip != nil {
			if err := checkIP(ip, blockPrivate); err != nil {
				return nil, err
			}
			return parent(ctx, network, addr)
		}
		ips, err := lookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("builtins: resolve %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("builtins: resolve %s: no addresses", host)
		}
		for _, ipa := range ips {
			if err := checkIP(ipa.IP, blockPrivate); err != nil {
				return nil, err
			}
		}
		var last error
		for _, ipa := range ips {
			target := net.JoinHostPort(ipa.IP.String(), port)
			conn, err := parent(ctx, network, target)
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}
}
