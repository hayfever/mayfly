package sources

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

// newHTTPClient builds one client per connection. Unless the connection sets
// allowPrivate, the client refuses loopback, private, link-local, carrier-grade
// NAT, and NAT64 destinations so a source cannot reach the cluster network.
// allowHTTP only permits the http scheme; it does not turn the guard off.
func newHTTPClient(conn Connection) (*http.Client, error) {
	base, err := url.Parse(conn.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("baseURL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("baseURL %q must be http or https", conn.BaseURL)
	}
	if base.Host == "" {
		return nil, errors.New("baseURL has no host")
	}
	if base.Scheme == "http" && !conn.AllowHTTP {
		return nil, fmt.Errorf("baseURL %q must use https unless allowHTTP is true", conn.BaseURL)
	}
	timeout := conn.ResolvedTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !conn.AllowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("refuse unresolved address %q", host)
			}
			if blockedAddress(ip) {
				return fmt.Errorf("address %s is not permitted (set allowPrivate on the connection to reach it)", ip)
			}
			return nil
		}
	}
	client := &http.Client{Timeout: timeout}
	client.Transport = &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" && (!conn.AllowHTTP || req.URL.Scheme != "http") {
			return errors.New("redirect must use https")
		}
		return nil
	}
	return client, nil
}

// nat64Prefixes translate to IPv4 inside the network, so they can reach the
// same private ranges the IPv4 checks block.
var nat64Prefixes = []*net.IPNet{
	mustCIDR("64:ff9b::/96"),
	mustCIDR("64:ff9b:1::/48"),
}

func mustCIDR(raw string) *net.IPNet {
	_, network, err := net.ParseCIDR(raw)
	if err != nil {
		panic(err)
	}
	return network
}

func blockedAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, prefix := range nat64Prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 0 {
			return true
		}
		// 100.64.0.0/10, carrier-grade NAT.
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
	}
	return false
}
