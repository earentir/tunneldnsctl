package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/spf13/cobra"
)

var (
	appVersion = "0.1.5"
)

// Config is the common struct: filled from flag defaults, then overlaid from JSON when --config is set.
type Config struct {
	ResolvPath     string
	Timeout        time.Duration
	Tries          int
	CheckDomain    string
	TunnelDNS      string
	StandardDNS    string
	TunnelSearch   []string
	StandardSearch []string
}

// configFile is the JSON schema for DNS and search (read from --config).
type configFile struct {
	TunnelDNS      string   `json:"tunnel_dns"`
	StandardDNS    string   `json:"standard_dns"`
	TunnelSearch   []string `json:"tunnel_search"`
	StandardSearch []string `json:"standard_search"`
}

type nsLine struct {
	idx       int
	raw       string
	isComment bool
	isNS      bool
	nsIP      string
}

// otherLine is a resolv.conf line that is not a nameserver (e.g. search, options, comment).
type otherLine struct {
	idx      int
	raw      string
	isSearch bool // true if line is "search domain ..."
}

func main() {
	var (
		timeout        time.Duration
		tries          int
		checkDomain    string
		resolvPath     string
		configPath     string
		tunnelDNS      string
		standardDNS    string
		tunnelSearch   []string
		standardSearch []string
	)
	rootCmd := &cobra.Command{
		Use:   "tunneldnsctl",
		Short: "Check tunnel DNS and update resolv.conf (tunnel first when up, standard when down)",
	}
	flags := rootCmd.PersistentFlags()
	flags.DurationVar(&timeout, "timeout", 400*time.Millisecond, "DNS check timeout per try")
	flags.IntVar(&tries, "tries", 2, "Number of DNS check attempts")
	flags.StringVar(&checkDomain, "check-domain", "internal.vodafoneinnovus.com", "Domain to resolve for reachability check")
	flags.StringVar(&resolvPath, "resolv-path", "/etc/resolv.conf", "Path to resolv.conf")
	flags.StringVar(&configPath, "config", "", "Path to JSON config (optional; overlays flag defaults)")
	flags.StringVar(&tunnelDNS, "tunnel-dns", "", "Tunnel DNS server IP (default from config file if --config set)")
	flags.StringVar(&standardDNS, "standard-dns", "", "Standard DNS server IP (default from config file if --config set)")
	flags.StringSliceVar(&tunnelSearch, "tunnel-search", nil, "Tunnel search domains (can be repeated; default from config file if --config set)")
	flags.StringSliceVar(&standardSearch, "standard-search", nil, "Standard search domains (can be repeated; default from config file if --config set)")

	versionFlag := rootCmd.PersistentFlags().BoolP("version", "v", false, "Print version and exit")
	rootCmd.PreRun = func(cmd *cobra.Command, args []string) {
		if *versionFlag {
			fmt.Println(appVersion)
			os.Exit(0)
		}
	}

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Check tunnel DNS and update resolv.conf",
	}
	updateResolvCmd := &cobra.Command{
		Use:   "resolv",
		Short: "Update resolv.conf: tunnel DNS first when reachable, else remove tunnel and add standard if no other DNS",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(loadConfigInput{
				resolvPath:     resolvPath,
				configPath:     configPath,
				timeout:        timeout,
				tries:          tries,
				checkDomain:    checkDomain,
				tunnelDNS:      tunnelDNS,
				standardDNS:    standardDNS,
				tunnelSearch:   tunnelSearch,
				standardSearch: standardSearch,
			})
			if err != nil {
				return err
			}
			return runResolv(cfg)
		},
	}
	updateCmd.AddCommand(updateResolvCmd)
	rootCmd.AddCommand(updateCmd)

	// update systemd — same tunnel/standard logic for systemd-resolved
	var resolvedConfPath string
	updateSystemdCmd := &cobra.Command{
		Use:   "systemd",
		Short: "Update systemd-resolved DNS (resolved.conf): tunnel first when up, standard when down",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(loadConfigInput{
				resolvPath:     resolvPath,
				configPath:     configPath,
				timeout:        timeout,
				tries:          tries,
				checkDomain:    checkDomain,
				tunnelDNS:      tunnelDNS,
				standardDNS:    standardDNS,
				tunnelSearch:   tunnelSearch,
				standardSearch: standardSearch,
			})
			if err != nil {
				return err
			}
			return runSystemd(cfg, resolvedConfPath)
		},
	}
	updateSystemdCmd.Flags().StringVar(&resolvedConfPath, "resolved-conf", "/etc/systemd/resolved.conf", "Path to systemd resolved.conf")
	updateCmd.AddCommand(updateSystemdCmd)

	// update nm — same tunnel/standard logic for NetworkManager connection DNS
	var nmConnection string
	updateNMCmd := &cobra.Command{
		Use:   "nm",
		Short: "Update NetworkManager connection DNS (via D-Bus): tunnel first when up, standard when down",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(loadConfigInput{
				resolvPath:     resolvPath,
				configPath:     configPath,
				timeout:        timeout,
				tries:          tries,
				checkDomain:    checkDomain,
				tunnelDNS:      tunnelDNS,
				standardDNS:    standardDNS,
				tunnelSearch:   tunnelSearch,
				standardSearch: standardSearch,
			})
			if err != nil {
				return err
			}
			return runNM(cfg, nmConnection)
		},
	}
	updateNMCmd.Flags().StringVar(&nmConnection, "connection", "", "NetworkManager connection name (default: first active)")
	updateCmd.AddCommand(updateNMCmd)

	// status — report DNS from resolv.conf, systemd-resolved, and NetworkManager
	var statusResolvPath string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show which DNS servers are in use (resolv.conf, systemd-resolved, NetworkManager)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(statusResolvPath)
		},
	}
	statusCmd.Flags().StringVar(&statusResolvPath, "resolv-path", "/etc/resolv.conf", "Path to resolv.conf to show")
	rootCmd.AddCommand(statusCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadConfigInput holds flag values; loadConfig uses them as defaults and overlays from JSON when --config is set.
type loadConfigInput struct {
	resolvPath     string
	configPath     string
	timeout        time.Duration
	tries          int
	checkDomain    string
	tunnelDNS      string
	standardDNS    string
	tunnelSearch   []string
	standardSearch []string
}

// loadConfig fills the common struct from flag defaults, then overlays from JSON when in.configPath is set.
func loadConfig(in loadConfigInput) (Config, error) {
	cfg := Config{
		ResolvPath:     in.resolvPath,
		Timeout:        in.timeout,
		Tries:          in.tries,
		CheckDomain:    in.checkDomain,
		TunnelDNS:      strings.TrimSpace(in.tunnelDNS),
		StandardDNS:    strings.TrimSpace(in.standardDNS),
		TunnelSearch:   trimSlice(in.tunnelSearch),
		StandardSearch: trimSlice(in.standardSearch),
	}
	if in.configPath != "" {
		data, err := os.ReadFile(in.configPath)
		if err != nil {
			return cfg, fmt.Errorf("reading config %s: %w", in.configPath, err)
		}
		var f configFile
		if err := json.Unmarshal(data, &f); err != nil {
			return cfg, fmt.Errorf("parsing config JSON: %w", err)
		}
		if v := strings.TrimSpace(f.TunnelDNS); v != "" {
			cfg.TunnelDNS = v
		}
		if v := strings.TrimSpace(f.StandardDNS); v != "" {
			cfg.StandardDNS = v
		}
		if len(f.TunnelSearch) > 0 {
			cfg.TunnelSearch = trimSlice(f.TunnelSearch)
		}
		if len(f.StandardSearch) > 0 {
			cfg.StandardSearch = trimSlice(f.StandardSearch)
		}
	}
	if cfg.TunnelDNS == "" {
		return cfg, errors.New("tunnel_dns is required (set --tunnel-dns or use --config with tunnel_dns)")
	}
	if cfg.StandardDNS == "" {
		return cfg, errors.New("standard_dns is required (set --standard-dns or use --config with standard_dns)")
	}
	return cfg, nil
}

func trimSlice(ss []string) []string {
	var out []string
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func runResolv(cfg Config) error {
	parsed, other, err := readResolv(cfg.ResolvPath)
	if err != nil {
		return err
	}

	tunnelUp := dnsOK(cfg.TunnelDNS, cfg)

	var newNS []string
	if tunnelUp {
		// Tunnel DNS first, then other active nameservers (excluding tunnel), each once.
		seen := map[string]bool{cfg.TunnelDNS: true}
		newNS = append(newNS, "nameserver "+cfg.TunnelDNS)
		for _, p := range parsed {
			if p.isNS && !p.isComment && p.nsIP != "" && !seen[p.nsIP] {
				seen[p.nsIP] = true
				newNS = append(newNS, "nameserver "+p.nsIP)
			}
		}
	} else {
		// Remove tunnel; if no other active nameserver, add standard.
		var otherActive []string
		for _, p := range parsed {
			if p.isNS && !p.isComment && p.nsIP != "" && p.nsIP != cfg.TunnelDNS {
				otherActive = append(otherActive, p.nsIP)
			}
		}
		seen := make(map[string]bool)
		for _, ip := range otherActive {
			if !seen[ip] {
				seen[ip] = true
				newNS = append(newNS, "nameserver "+ip)
			}
		}
		if len(newNS) == 0 {
			newNS = append(newNS, "nameserver "+cfg.StandardDNS)
		}
	}

	// Search line: when tunnel is up use tunnel_search then standard_search; when down use standard_search only.
	var searchDomains []string
	if tunnelUp {
		searchDomains = append(append([]string{}, cfg.TunnelSearch...), cfg.StandardSearch...)
	} else {
		searchDomains = append([]string{}, cfg.StandardSearch...)
	}

	// First and last nameserver line indices (for ordering other lines).
	firstNSIdx := -1
	lastNSIdx := -1
	for _, p := range parsed {
		if p.isNS {
			if firstNSIdx < 0 {
				firstNSIdx = p.idx
			}
			lastNSIdx = p.idx
		}
	}

	// Other lines before first NS (excluding old search lines — we emit our own).
	var before []string
	for _, o := range other {
		if o.isSearch {
			continue
		}
		if firstNSIdx < 0 || o.idx < firstNSIdx {
			before = append(before, o.raw)
		}
	}

	// Other lines after last NS (excluding old search lines). When there are no NS lines, nothing is "after".
	var after []string
	if lastNSIdx >= 0 {
		for _, o := range other {
			if o.idx > lastNSIdx && !o.isSearch {
				after = append(after, o.raw)
			}
		}
	}

	var out []string
	out = append(out, before...)
	out = append(out, newNS...)
	if len(searchDomains) > 0 {
		out = append(out, "search "+strings.Join(searchDomains, " "))
	}
	out = append(out, after...)

	return writeFileAtomicIfChanged(cfg.ResolvPath, strings.Join(out, "\n")+"\n")
}

// systemd-resolved: [Resolve] section with DNS=, FallbackDNS=, Domains= (search).
func runSystemd(cfg Config, resolvedConfPath string) error {
	content, err := os.ReadFile(resolvedConfPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", resolvedConfPath, err)
	}
	dnsIPs, fallbackIPs, _, rest, err := parseResolvedConf(string(content))
	if err != nil {
		return err
	}
	// Current primary DNS is first in DNS= (or first in FallbackDNS= if DNS= empty).
	allCurrent := append(dnsIPs, fallbackIPs...)
	tunnelUp := dnsOK(cfg.TunnelDNS, cfg)

	var newDNS, newFallback []string
	var newDomains []string
	if tunnelUp {
		seen := map[string]bool{cfg.TunnelDNS: true}
		newDNS = []string{cfg.TunnelDNS}
		for _, ip := range allCurrent {
			if ip != cfg.TunnelDNS && !seen[ip] {
				seen[ip] = true
				newFallback = append(newFallback, ip)
			}
		}
		if !seen[cfg.StandardDNS] {
			newFallback = append(newFallback, cfg.StandardDNS)
		}
		newDomains = append(append([]string{}, cfg.TunnelSearch...), cfg.StandardSearch...)
	} else {
		for _, ip := range allCurrent {
			if ip != cfg.TunnelDNS {
				newDNS = append(newDNS, ip)
			}
		}
		if len(newDNS) == 0 {
			newDNS = []string{cfg.StandardDNS}
		}
		newFallback = nil
		newDomains = append([]string{}, cfg.StandardSearch...)
	}
	newContent := strings.Join(replaceResolvedRest(rest, newDNS, newFallback, newDomains), "\n") + "\n"
	return writeFileAtomicIfChanged(resolvedConfPath, newContent)
}

func parseResolvedConf(content string) (dns, fallback, domains []string, rest []string, err error) {
	lines := strings.Split(content, "\n")
	inResolve := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			rest = append(rest, line)
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			inResolve = trimmed == "[Resolve]"
			rest = append(rest, line)
			continue
		}
		if inResolve {
			if strings.HasPrefix(trimmed, "DNS=") {
				dns = parseResolvedValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "DNS=")))
				rest = append(rest, line)
				continue
			}
			if strings.HasPrefix(trimmed, "FallbackDNS=") {
				fallback = parseResolvedValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "FallbackDNS=")))
				rest = append(rest, line)
				continue
			}
			if strings.HasPrefix(trimmed, "Domains=") {
				domains = parseResolvedValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "Domains=")))
				rest = append(rest, line)
				continue
			}
		}
		rest = append(rest, line)
	}
	return dns, fallback, domains, rest, nil
}

func parseResolvedValue(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Fields(v)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "[") {
			if end := strings.Index(p, "]"); end > 0 {
				out = append(out, p[1:end])
			} else {
				out = append(out, p)
			}
			continue
		}
		if idx := strings.IndexAny(p, ":%#"); idx > 0 {
			out = append(out, p[:idx])
		} else {
			out = append(out, p)
		}
	}
	return out
}

func replaceResolvedRest(rest []string, dns, fallback, domains []string) []string {
	inResolve := false
	var out []string
	for _, line := range rest {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[Resolve]" {
			inResolve = true
			out = append(out, line)
			continue
		}
		if inResolve && (strings.HasPrefix(trimmed, "DNS=") || strings.HasPrefix(trimmed, "FallbackDNS=") || strings.HasPrefix(trimmed, "Domains=")) {
			if strings.HasPrefix(trimmed, "DNS=") {
				out = append(out, "DNS="+strings.Join(dns, " "))
			} else if strings.HasPrefix(trimmed, "FallbackDNS=") {
				out = append(out, "FallbackDNS="+strings.Join(fallback, " "))
			} else {
				out = append(out, "Domains="+strings.Join(domains, " "))
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") && trimmed != "[Resolve]" {
			inResolve = false
		}
		out = append(out, line)
	}
	return out
}

const (
	nmBusName                      = "org.freedesktop.NetworkManager"
	nmObjectPath                   = "/org/freedesktop/NetworkManager"
	nmSettingsPath                  = "/org/freedesktop/NetworkManager/Settings"
	nmSettingsInterface             = "org.freedesktop.NetworkManager.Settings"
	nmSettingsConnInterface         = "org.freedesktop.NetworkManager.Settings.Connection"
	nmConnectionActiveInterface     = "org.freedesktop.NetworkManager.Connection.Active"
)

func runNM(cfg Config, connectionName string) error {
	conn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("dbus: %w", err)
	}
	settingsPath, activePath, err := nmResolveConnection(conn, connectionName)
	if err != nil {
		return err
	}
	currentDNS, err := nmGetConnectionDNS(conn, settingsPath)
	if err != nil {
		return err
	}
	tunnelUp := dnsOK(cfg.TunnelDNS, cfg)

	var newDNS []string
	if tunnelUp {
		seen := map[string]bool{cfg.TunnelDNS: true}
		newDNS = append(newDNS, cfg.TunnelDNS)
		for _, ip := range currentDNS {
			if ip != "" && !seen[ip] {
				seen[ip] = true
				newDNS = append(newDNS, ip)
			}
		}
		if !seen[cfg.StandardDNS] {
			newDNS = append(newDNS, cfg.StandardDNS)
		}
	} else {
		for _, ip := range currentDNS {
			if ip != cfg.TunnelDNS && ip != "" {
				newDNS = append(newDNS, ip)
			}
		}
		if len(newDNS) == 0 {
			newDNS = []string{cfg.StandardDNS}
		}
	}
	if err := nmSetConnectionDNS(conn, settingsPath, newDNS); err != nil {
		return err
	}
	if activePath != "" {
		return nmReactivateConnection(conn, settingsPath, activePath)
	}
	return nil
}

func nmGetActiveDNS() ([]string, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}
	_, activePath, err := nmResolveConnection(conn, "")
	if err != nil {
		return nil, err
	}
	obj := conn.Object(nmBusName, activePath)
	var connectionPath dbus.ObjectPath
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, nmConnectionActiveInterface, "Connection").Store(&connectionPath); err != nil {
		return nil, err
	}
	return nmGetConnectionDNS(conn, connectionPath)
}

func nmResolveConnection(conn *dbus.Conn, byName string) (settingsPath, activePath dbus.ObjectPath, err error) {
	if byName == "" {
		obj := conn.Object(nmBusName, nmObjectPath)
		var activePaths []dbus.ObjectPath
		if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager", "ActiveConnections").Store(&activePaths); err != nil {
			return "", "", fmt.Errorf("get ActiveConnections: %w", err)
		}
		if len(activePaths) == 0 {
			return "", "", errors.New("no active NetworkManager connection found")
		}
		activePath = activePaths[0]
		obj = conn.Object(nmBusName, activePath)
		if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, nmConnectionActiveInterface, "Connection").Store(&settingsPath); err != nil {
			return "", "", fmt.Errorf("get Connection from active: %w", err)
		}
		return settingsPath, activePath, nil
	}
	obj := conn.Object(nmBusName, nmSettingsPath)
	var connectionPaths []dbus.ObjectPath
	if err := obj.Call(nmSettingsInterface+".GetConnections", 0).Store(&connectionPaths); err != nil {
		return "", "", fmt.Errorf("GetConnections: %w", err)
	}
	for _, p := range connectionPaths {
		id, _ := nmGetConnectionID(conn, p)
		if id == byName {
			var activePaths []dbus.ObjectPath
			nmObj := conn.Object(nmBusName, nmObjectPath)
			_ = nmObj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager", "ActiveConnections").Store(&activePaths)
			for _, ap := range activePaths {
				var cp dbus.ObjectPath
				acObj := conn.Object(nmBusName, ap)
				if acObj.Call("org.freedesktop.DBus.Properties.Get", 0, nmConnectionActiveInterface, "Connection").Store(&cp) == nil && cp == p {
					return p, ap, nil
				}
			}
			return p, "", nil
		}
	}
	return "", "", fmt.Errorf("connection %q not found", byName)
}

func nmGetConnectionID(conn *dbus.Conn, path dbus.ObjectPath) (string, error) {
	settings, err := nmGetConnectionSettings(conn, path)
	if err != nil {
		return "", err
	}
	connSection, ok := settings["connection"]
	if !ok {
		return "", nil
	}
	if v, ok := connSection["id"]; ok {
		var id string
		if err := v.Store(&id); err == nil {
			return id, nil
		}
	}
	return "", nil
}

func nmGetConnectionSettings(conn *dbus.Conn, path dbus.ObjectPath) (map[string]map[string]dbus.Variant, error) {
	obj := conn.Object(nmBusName, path)
	var settings map[string]map[string]dbus.Variant
	if err := obj.Call(nmSettingsConnInterface+".GetSettings", 0).Store(&settings); err != nil {
		return nil, err
	}
	return settings, nil
}

func nmGetConnectionDNS(conn *dbus.Conn, path dbus.ObjectPath) ([]string, error) {
	settings, err := nmGetConnectionSettings(conn, path)
	if err != nil {
		return nil, err
	}
	ipv4, ok := settings["ipv4"]
	if !ok {
		return nil, nil
	}
	if v, ok := ipv4["dns-data"]; ok {
		var list []string
		if err := v.Store(&list); err == nil && len(list) > 0 {
			return list, nil
		}
	}
	if v, ok := ipv4["dns"]; ok {
		var list []uint32
		if err := v.Store(&list); err == nil && len(list) > 0 {
			out := make([]string, 0, len(list))
			for _, u := range list {
				out = append(out, nmUint32ToIPv4(u))
			}
			return out, nil
		}
	}
	return nil, nil
}

func nmUint32ToIPv4(u uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", u&0xff, (u>>8)&0xff, (u>>16)&0xff, (u>>24)&0xff)
}

func nmSetConnectionDNS(conn *dbus.Conn, path dbus.ObjectPath, ips []string) error {
	settings, err := nmGetConnectionSettings(conn, path)
	if err != nil {
		return err
	}
	ipv4, ok := settings["ipv4"]
	if !ok {
		ipv4 = make(map[string]dbus.Variant)
		settings["ipv4"] = ipv4
	}
	u32s := make([]uint32, 0, len(ips))
	for _, ip := range ips {
		u32s = append(u32s, nmIPv4ToUint32(ip))
	}
	ipv4["dns"] = dbus.MakeVariant(u32s)
	delete(ipv4, "dns-data")
	obj := conn.Object(nmBusName, path)
	if err := obj.Call(nmSettingsConnInterface+".Update", 0, settings).Err; err != nil {
		return fmt.Errorf("update connection: %w", err)
	}
	return nil
}

func nmIPv4ToUint32(ip string) uint32 {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return 0
	}
	v4 := parsed.To4()
	return uint32(v4[0]) | uint32(v4[1])<<8 | uint32(v4[2])<<16 | uint32(v4[3])<<24
}

func nmReactivateConnection(conn *dbus.Conn, settingsPath, activePath dbus.ObjectPath) error {
	if activePath == "" {
		return errors.New("cannot re-activate: connection is not active")
	}
	obj := conn.Object(nmBusName, activePath)
	var devices []dbus.ObjectPath
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, nmConnectionActiveInterface, "Devices").Store(&devices); err != nil || len(devices) == 0 {
		return fmt.Errorf("get Devices from active connection: %w", err)
	}
	devicePath := devices[0]
	nmObj := conn.Object(nmBusName, nmObjectPath)
	if err := nmObj.Call("org.freedesktop.NetworkManager.DeactivateConnection", 0, activePath).Err; err != nil {
		return fmt.Errorf("DeactivateConnection: %w", err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := nmObj.Call("org.freedesktop.NetworkManager.ActivateConnection", 0, settingsPath, devicePath, dbus.ObjectPath("/")).Err; err != nil {
		return fmt.Errorf("ActivateConnection: %w", err)
	}
	return nil
}

func runStatus(resolvPath string) error {
	sources := make(map[string][]string)

	parsed, _, err := readResolv(resolvPath)
	if err == nil {
		var ns []string
		for _, p := range parsed {
			if p.isNS && !p.isComment && p.nsIP != "" {
				ns = append(ns, p.nsIP)
			}
		}
		if len(ns) > 0 {
			sources["resolv.conf"] = ns
		}
	}

	const systemdResolvPath = "/run/systemd/resolve/resolv.conf"
	if parsed, _, err := readResolv(systemdResolvPath); err == nil {
		var ns []string
		for _, p := range parsed {
			if p.isNS && !p.isComment && p.nsIP != "" {
				ns = append(ns, p.nsIP)
			}
		}
		if len(ns) > 0 {
			sources["systemd-resolved"] = ns
		}
	}

	if ns, err := nmGetActiveDNS(); err == nil && len(ns) > 0 {
		sources["NetworkManager"] = ns
	}

	if len(sources) == 0 {
		fmt.Println("No DNS sources found (resolv.conf missing or empty, or systemd-resolved/NetworkManager not available).")
		return nil
	}
	for name, ips := range sources {
		fmt.Printf("%s:\t%s\n", name, strings.Join(ips, " "))
	}
	return nil
}

func dnsOK(server string, cfg Config) bool {
	if cfg.Tries < 1 {
		cfg.Tries = 1
	}

	for i := 0; i < cfg.Tries; i++ {
		if dnsOKOnce(server, cfg) {
			return true
		}
		// small delay between tries (bounded, low impact)
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func dnsOKOnce(server string, cfg Config) bool {
	r := net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: cfg.Timeout}
			return d.DialContext(ctx, "udp", net.JoinHostPort(server, "53"))
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// A response (NOERROR or NXDOMAIN) still indicates reachability; net.Resolver
	// surfaces NXDOMAIN as an error, so we accept many error types as "reachable"
	// only if we got a response. Unfortunately net.Resolver doesn't expose that
	// directly, so we use a pragmatic approach: treat timeout/conn errors as fail;
	// otherwise assume reachable.
	_, err := r.LookupHost(ctx, cfg.CheckDomain)
	if err == nil {
		return true
	}

	// Only consider definite transport failures as unreachable.
	// For most other resolver-level errors, the server is up.
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	// If the UDP dial itself fails, net.Resolver usually returns a net.OpError.
	var opErr *net.OpError
	return !errors.As(err, &opErr)
}

func readResolv(path string) (parsed []nsLine, other []otherLine, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	i := 0
	for sc.Scan() {
		raw := sc.Text()
		trimLeft := strings.TrimLeft(raw, " \t")
		trim := strings.TrimSpace(trimLeft)
		uncomment := trim
		if strings.HasPrefix(trim, "#") {
			uncomment = strings.TrimSpace(strings.TrimPrefix(trim, "#"))
		}

		if strings.HasPrefix(uncomment, "nameserver") {
			fields := strings.Fields(uncomment)
			if len(fields) >= 2 {
				parsed = append(parsed, nsLine{
					idx:       i,
					raw:       raw,
					isComment: strings.HasPrefix(trim, "#"),
					isNS:      true,
					nsIP:      fields[1],
				})
				i++
				continue
			}
		}
		isSearch := strings.HasPrefix(uncomment, "search ")
		other = append(other, otherLine{idx: i, raw: raw, isSearch: isSearch})
		i++
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return parsed, other, nil
}

func writeFileAtomicIfChanged(path string, content string) error {
	old, _ := os.ReadFile(path)
	if string(old) == content {
		return nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".resolvconf-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Preserve mode if possible; otherwise default 0644.
	mode := os.FileMode(0644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode()
	}
	_ = os.Chmod(tmpName, mode)

	return os.Rename(tmpName, path)
}
