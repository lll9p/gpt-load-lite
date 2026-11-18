package outboundproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSubscriptionBytes = 1 << 20

func parseSubscription(body []byte, format, protocol string) ([]string, error) {
	if len(body) > maxSubscriptionBytes || !utf8.Valid(body) {
		return nil, subscriptionError("invalid subscription content")
	}
	seen := make(map[proxyIdentity]string)
	var rows []string
	count := 0
	for i, line := range strings.Split(strings.TrimPrefix(string(body), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		count++
		if count > 2000 {
			return nil, subscriptionError("subscription exceeds 2000 entries")
		}
		raw := line
		if format == "webshare" {
			raw = webshareURL(line, protocol)
		}
		normalized, err := normalize(raw)
		if err != nil {
			return nil, subscriptionError(fmt.Sprintf("invalid subscription line %d", i+1))
		}
		u, _ := url.Parse(normalized)
		if utf8.RuneCountInString(u.Host) > 255 {
			return nil, subscriptionError(fmt.Sprintf("invalid subscription line %d", i+1))
		}
		key, canonical := identify(normalized)
		if old, ok := seen[key]; ok {
			if old != canonical {
				return nil, subscriptionError("subscription contains conflicting credentials")
			}
			continue
		}
		seen[key] = canonical
		rows = append(rows, normalized)
	}
	if len(rows) == 0 {
		return nil, subscriptionError("subscription is empty")
	}
	return rows, nil
}

func webshareURL(line, protocol string) string {
	host, suffix, ok := strings.Cut(line, ":")
	if strings.HasPrefix(line, "[") {
		host, suffix, ok = strings.Cut(line[1:], "]:")
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is6() {
			return ""
		}
	} else if parts := strings.SplitN(suffix, ":", 3); len(parts) != 3 || !validPort(parts[0]) {
		// Accept unbracketed IPv6 only when its host/port boundary is unambiguous.
		ok = false
		for i := len(host); i < min(len(line), 46); i++ {
			if line[i] != ':' {
				continue
			}
			ip, err := netip.ParseAddr(line[:i])
			if err != nil || !ip.Is6() {
				continue
			}
			parts := strings.SplitN(line[i+1:], ":", 3)
			if len(parts) != 3 || !validPort(parts[0]) {
				continue
			}
			if ok {
				return ""
			}
			host, suffix, ok = line[:i], line[i+1:], true
		}
	}
	parts := strings.SplitN(suffix, ":", 3)
	if !ok || host == "" || len(parts) != 3 || !validPort(parts[0]) || parts[1] == "" || strings.ContainsAny(host+suffix, "\x00\r\n") {
		return ""
	}
	return (&url.URL{Scheme: protocol, Host: net.JoinHostPort(host, parts[0]), User: url.UserPassword(parts[1], parts[2])}).String()
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

type proxyIdentity struct{ protocol, host, port, username string }

// identify accepts only normalized URLs. normalize strips default ports.
func identify(raw string) (proxyIdentity, string) {
	u, _ := url.Parse(raw)
	host := strings.TrimSuffix(u.Hostname(), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	port := u.Port()
	key := proxyIdentity{u.Scheme, host, port, u.User.Username()}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	return key, u.String()
}

func safeTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.Contains(raw, "#") || strings.ContainsAny(u.Hostname(), "%\\ \t\r\n") || strings.HasSuffix(u.Host, ":") || (u.Port() != "" && !validPort(u.Port())) {
		return nil, subscriptionError("invalid subscription download URL")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicIP(ip) {
		return nil, subscriptionError("subscription download requires a public address")
	}
	return u, nil
}

// Global unicast includes special-purpose ranges that may be routed locally.
var specialPrefixes = func() []netip.Prefix {
	ranges := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "168.63.129.16/32",
		"2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20",
	}
	prefixes := make([]netip.Prefix, 0, len(ranges))
	for _, r := range ranges {
		prefixes = append(prefixes, netip.MustParsePrefix(r))
	}
	return prefixes
}()
var globalIPv6 = netip.MustParsePrefix("2000::/3")

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalIPv6.Contains(ip)) {
		return false
	}
	for _, prefix := range specialPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var errDownloadTarget = errors.New("subscription download requires a public address")

func dialSubscription(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errDownloadTarget
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("subscription host lookup failed")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errDownloadTarget
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("subscription connection failed")
}

func downloadSubscription(ctx context.Context, in SubscriptionInput) ([]string, error) {
	u, err := safeTarget(in.URL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: dialSubscription, DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, subscriptionError("invalid subscription download URL")
	}
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errDownloadTarget) {
			return nil, subscriptionError(errDownloadTarget.Error())
		}
		return nil, subscriptionError("subscription download failed")
	}
	defer res.Body.Close()
	return readSubscriptionResponse(res, in)
}

func readSubscriptionResponse(res *http.Response, in SubscriptionInput) ([]string, error) {
	if res.StatusCode != http.StatusOK {
		return nil, subscriptionError(fmt.Sprintf("subscription server returned HTTP %d", res.StatusCode))
	}
	if res.Header.Get("Content-Range") != "" {
		return nil, subscriptionError("subscription server returned partial content")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, subscriptionError("subscription download failed")
	}
	return parseSubscription(body, in.Format, in.Protocol)
}
