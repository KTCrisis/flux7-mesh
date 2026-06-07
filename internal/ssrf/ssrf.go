// Package ssrf provides an HTTP client hardened against Server-Side Request
// Forgery. The guard runs at dial time, so it defeats DNS rebinding (the
// hostname is resolved and checked on the actual connection, not earlier) and
// inspects every resolved address, not just the first.
package ssrf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// CheckURL rejects URLs that are obviously unsafe before any network call:
// non-HTTP(S) schemes and the literal "localhost". IP/host reachability is
// enforced later by the guarded dialer.
func CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("disallowed scheme %q", u.Scheme)
	}
	if u.Hostname() == "localhost" {
		return fmt.Errorf("localhost not allowed")
	}
	return nil
}

// isBlocked reports whether an IP must not be dialed: loopback, private
// (RFC1918), link-local (incl. the cloud metadata 169.254.169.254), or the
// unspecified address.
func isBlocked(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// Client returns an *http.Client whose dialer resolves the target host, rejects
// the connection if ANY resolved address is blocked, and dials a vetted address
// directly — so the IP that passed the check is the IP actually contacted (no
// TOCTOU window for DNS rebinding).
func Client(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("ssrf: no address for %s", host)
			}
			for _, ip := range ips {
				if isBlocked(ip.IP) {
					return nil, fmt.Errorf("ssrf: blocked address %s for %s", ip.IP, host)
				}
			}
			// All resolved addresses are safe; dial the first one directly.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}
