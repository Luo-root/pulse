package builtins

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

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
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// 阿里云 metadata
	if ip.Equal(net.ParseIP("100.100.100.200")) {
		return true
	}
	return false
}

func checkHost(host string, blockPrivate bool) error {
	h := host
	if h2, _, err := net.SplitHostPort(host); err == nil {
		h = h2
	}
	h = strings.Trim(h, "[]")
	if ip := net.ParseIP(h); ip != nil {
		return checkIP(ip, blockPrivate)
	}
	ips, err := net.LookupIP(h)
	if err != nil {
		return fmt.Errorf("builtins: resolve %s: %w", h, err)
	}
	for _, ip := range ips {
		if err := checkIP(ip, blockPrivate); err != nil {
			return err
		}
	}
	return nil
}

func checkIP(ip net.IP, blockPrivate bool) error {
	if blockedMetadataIP(ip) {
		return fmt.Errorf("builtins: refusing metadata/link-local address %s", ip)
	}
	if blockPrivate && (ip.IsPrivate() || ip.IsLoopback()) {
		return fmt.Errorf("builtins: refusing private/loopback address %s", ip)
	}
	return nil
}
