package builtins

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestCheckHostDNSRefusesMetadata(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if host != "evil.example" {
			t.Fatalf("host=%s", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	err := checkHost(context.Background(), "evil.example", false)
	if err == nil || (!strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "link-local")) {
		t.Fatalf("want metadata refuse, got %v", err)
	}
}

func TestCheckHostDNSMappedIPv6(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("::ffff:169.254.169.254")}}, nil
	}
	err := checkHost(context.Background(), "mapped.example", false)
	if err == nil || (!strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "link-local")) {
		t.Fatalf("mapped IPv6 should refuse, got %v", err)
	}
}

func TestCheckHostDNSBlockPrivate(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
	}
	if err := checkHost(context.Background(), "corp.example", false); err != nil {
		t.Fatalf("private allowed by default: %v", err)
	}
	err := checkHost(context.Background(), "corp.example", true)
	if err == nil || (!strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback")) {
		t.Fatalf("BlockPrivate should refuse RFC1918, got %v", err)
	}
}

func TestGuardedDialPinsResolvedIP(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	var got string
	dial := guardedDial(func(_ context.Context, network, addr string) (net.Conn, error) {
		got = addr
		return nil, errors.New("stop")
	}, false)
	_, err := dial(context.Background(), "tcp", "safe.example:443")
	if err == nil || err.Error() != "stop" {
		t.Fatalf("err=%v", err)
	}
	if got != "8.8.8.8:443" {
		t.Fatalf("should pin resolved IP, got %q", got)
	}
}

func TestGuardedDialDNSRefusesMetadata(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
	}
	called := false
	dial := guardedDial(func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("should not dial")
	}, false)
	_, err := dial(context.Background(), "tcp", "evil.example:80")
	if err == nil || (!strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "link-local")) {
		t.Fatalf("want metadata refuse, got %v", err)
	}
	if called {
		t.Fatal("must not dial blocked IP")
	}
}

func TestCheckIPAliyunMetadata(t *testing.T) {
	err := checkIP(net.ParseIP("100.100.100.200"), false)
	if err == nil || (!strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "link-local")) {
		t.Fatalf("%v", err)
	}
}

func TestCheckHostHonorsCancel(t *testing.T) {
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := checkHost(ctx, "slow.example", false)
	if err == nil || (!strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "cancelled")) {
		t.Fatalf("canceled ctx should fail DNS, got %v", err)
	}
}
