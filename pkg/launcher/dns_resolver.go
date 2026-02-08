package launcher

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	dnsMu     sync.RWMutex
	dnsSource string
	dnsTried  []string
	dnsRR     uint32
)

func configureLauncherDNS(override []string) error {
	servers, source, warnings, err := pickDNSServers(override)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Printf("DNS 警告: %s\n", w)
	}

	dnsMu.Lock()
	dnsSource = source
	dnsTried = append([]string(nil), servers...)
	dnsMu.Unlock()

	if len(servers) == 0 {
		return nil
	}

	localServers := append([]string(nil), servers...)
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &net.Dialer{}
			n := len(localServers)
			start := int(atomic.AddUint32(&dnsRR, 1)-1) % n
			var lastErr error
			for i := 0; i < n; i++ {
				server := localServers[(start+i)%n]
				conn, err := d.DialContext(ctx, "udp", server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no DNS server available")
			}
			return nil, lastErr
		},
	}
	return nil
}

func dnsResolverDebugInfo() string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	if len(dnsTried) == 0 {
		if dnsSource == "" {
			return "system default resolver"
		}
		return dnsSource
	}
	return fmt.Sprintf("%s (%s)", dnsSource, strings.Join(dnsTried, ", "))
}

func launcherDNSServerForChild() string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	if len(dnsTried) == 0 {
		return ""
	}

	for _, server := range dnsTried {
		host, _, err := net.SplitHostPort(server)
		if err != nil {
			continue
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			continue
		}
		return server
	}

	return dnsTried[0]
}

func pickDNSServers(override []string) ([]string, string, []string, error) {
	if len(override) > 0 {
		s, warns, err := normalizeDNSServers(override)
		if err != nil {
			return nil, "", nil, err
		}
		return s, "info.json dns_servers", warns, nil
	}

	if runtime.GOOS == "android" {
		s, warns, err := normalizeDNSServers(androidSystemDNSServers())
		if err == nil && len(s) > 0 {
			return s, "android getprop net.dns1..net.dns4", warns, nil
		}
		fallback := publicDNSServers()
		return fallback, "android fallback public DNS", nil, nil
	}

	if hasUsableResolvConf() {
		return nil, "system default resolver", nil, nil
	}

	// Keep default behavior on macOS/windows; only apply this fallback on linux-like environments.
	if runtime.GOOS == "linux" {
		return publicDNSServers(), "/etc/resolv.conf missing or unusable", nil, nil
	}
	return nil, "system default resolver", nil, nil
}

func normalizeDNSServers(raw []string) ([]string, []string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	warnings := []string{}
	for _, item := range raw {
		s, err := normalizeDNSServer(item)
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 && len(raw) > 0 {
		return nil, warnings, fmt.Errorf("dns_servers 全部無效: %s", strings.Join(warnings, "; "))
	}
	return out, warnings, nil
}

func normalizeDNSServer(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("空白 DNS server 設定")
	}

	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		ip := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		if parsed := net.ParseIP(ip); parsed != nil {
			return net.JoinHostPort(parsed.String(), "53"), nil
		}
	}

	if parsed := net.ParseIP(s); parsed != nil {
		return net.JoinHostPort(parsed.String(), "53"), nil
	}

	if host, port, err := net.SplitHostPort(s); err == nil {
		host = strings.Trim(host, "[]")
		if host == "" || port == "" {
			return "", fmt.Errorf("無效 DNS server: %q", raw)
		}
		return net.JoinHostPort(host, port), nil
	}

	if !strings.Contains(s, ":") {
		return net.JoinHostPort(s, "53"), nil
	}

	return "", fmt.Errorf("無效 DNS server: %q", raw)
}

func androidSystemDNSServers() []string {
	keys := []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		cmd := exec.Command("getprop", key)
		b, err := cmd.Output()
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func hasUsableResolvConf() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ns := strings.TrimSpace(fields[1])
		if ns == "" {
			continue
		}
		ip := net.ParseIP(strings.Trim(ns, "[]"))
		if ip != nil && ip.IsLoopback() {
			continue
		}
		return true
	}
	return false
}

func publicDNSServers() []string {
	return []string{
		"8.8.8.8:53",
		"1.1.1.1:53",
		"[2001:4860:4860::8888]:53",
		"[2606:4700:4700::1111]:53",
	}
}
