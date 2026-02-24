package main

import (
	"bufio"
	"context"
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

type FailAction int

const (
	// Default: only touch the *first active* nameserver line.
	// If it is not reachable, comment it out and leave everything else as-is.
	FailActionCommentOutFirst FailAction = iota

	// Optional: replace ALL nameserver lines with ReplacementDNS (in that order),
	// and leave everything else (search/options/etc) as-is.
	FailActionReplaceNameservers
)

var (
	appVersion = "0.1.0"
)

type Config struct {
	ResolvPath string

	// How we decide if a DNS server is "accessible".
	Timeout     time.Duration
	Tries       int
	CheckDomain string // e.g. internal.vodafoneinnovus.com

	// What to do when the first active nameserver is not reachable.
	OnFail         FailAction
	ReplacementDNS []string // used only when OnFail == FailActionReplaceNameservers
}

type nsLine struct {
	idx        int
	raw        string
	isComment  bool
	isNS       bool
	nsIP       string
	leadingWS  string
	commentWS  string
	commentTag string
}

const (
	defaultResolvPath       = "/etc/resolv.conf"
	defaultResolvedConfPath = "/etc/systemd/resolved.conf"
	defaultTimeout          = 400 * time.Millisecond
	defaultTries            = 2
	defaultCheckDomain      = "internal.vodafoneinnovus.com"
	defaultOnFail           = "comment-out-first"
	defaultReplacementDNS   = "192.168.178.22"
)

func main() {
	var (
		timeout        time.Duration
		tries          int
		checkDomain    string
		onFailStr      string
		replacementDNS []string
	)

	rootCmd := &cobra.Command{
		Use:   "updatednswithcheck",
		Short: "Check DNS reachability and update resolv.conf, systemd-resolved, or NetworkManager",
	}
	flags := rootCmd.PersistentFlags()
	flags.DurationVar(&timeout, "timeout", defaultTimeout, "DNS check timeout per try")
	flags.IntVar(&tries, "tries", defaultTries, "Number of DNS check attempts")
	flags.StringVar(&checkDomain, "check-domain", defaultCheckDomain, "Domain to resolve for reachability check")
	flags.StringVar(&onFailStr, "on-fail", defaultOnFail, "Action when first nameserver is unreachable: comment-out-first | replace-nameservers")
	flags.StringSliceVar(&replacementDNS, "replacement-dns", []string{defaultReplacementDNS}, "DNS servers to use when on-fail=replace-nameservers (can be repeated)")

	versionFlag := rootCmd.PersistentFlags().BoolP("version", "v", false, "Print version and exit")
	rootCmd.PreRun = func(cmd *cobra.Command, args []string) {
		if *versionFlag {
			fmt.Println(appVersion)
			os.Exit(0)
		}
	}

	// update command and subcommands
	var resolvPath, resolvedConfPath, nmConnection string
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Check first DNS and apply fail action if unreachable",
	}
	updateResolvCmd := &cobra.Command{
		Use:   "resolv",
		Short: "Update /etc/resolv.conf (or --resolv-path)",
		RunE: func(cmd *cobra.Command, args []string) error {
			onFail, err := parseFailAction(onFailStr)
			if err != nil {
				return err
			}
			cfg := Config{
				ResolvPath:     resolvPath,
				Timeout:        timeout,
				Tries:          tries,
				CheckDomain:    checkDomain,
				OnFail:         onFail,
				ReplacementDNS: replacementDNS,
			}
			return runResolv(cfg)
		},
	}
	updateResolvCmd.Flags().StringVar(&resolvPath, "resolv-path", defaultResolvPath, "Path to resolv.conf")

	updateSystemdCmd := &cobra.Command{
		Use:   "systemd",
		Short: "Update systemd-resolved DNS (resolved.conf)",
		RunE: func(cmd *cobra.Command, args []string) error {
			onFail, err := parseFailAction(onFailStr)
			if err != nil {
				return err
			}
			cfg := Config{
				ResolvPath:     "", // unused
				Timeout:        timeout,
				Tries:          tries,
				CheckDomain:    checkDomain,
				OnFail:         onFail,
				ReplacementDNS: replacementDNS,
			}
			return runSystemd(cfg, resolvedConfPath)
		},
	}
	updateSystemdCmd.Flags().StringVar(&resolvedConfPath, "resolved-conf", defaultResolvedConfPath, "Path to systemd resolved.conf")

	updateNMCmd := &cobra.Command{
		Use:   "nm",
		Short: "Update NetworkManager connection DNS (via D-Bus)",
		RunE: func(cmd *cobra.Command, args []string) error {
			onFail, err := parseFailAction(onFailStr)
			if err != nil {
				return err
			}
			cfg := Config{
				Timeout:        timeout,
				Tries:          tries,
				CheckDomain:    checkDomain,
				OnFail:         onFail,
				ReplacementDNS: replacementDNS,
			}
			return runNM(cfg, nmConnection)
		},
	}
	updateNMCmd.Flags().StringVar(&nmConnection, "connection", "", "NetworkManager connection name (default: first active)")

	updateCmd.AddCommand(updateResolvCmd, updateSystemdCmd, updateNMCmd)
	rootCmd.AddCommand(updateCmd)

	// status command
	var statusResolvPath string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show which DNS servers are in use (resolv.conf, systemd-resolved, NetworkManager)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(statusResolvPath)
		},
	}
	statusCmd.Flags().StringVar(&statusResolvPath, "resolv-path", defaultResolvPath, "Path to resolv.conf to show")
	rootCmd.AddCommand(statusCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseFailAction(s string) (FailAction, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "comment-out-first":
		return FailActionCommentOutFirst, nil
	case "replace-nameservers":
		return FailActionReplaceNameservers, nil
	default:
		return 0, fmt.Errorf("invalid on-fail %q: use comment-out-first or replace-nameservers", s)
	}
}

func runResolv(cfg Config) error {
	lines, parsed, err := readResolv(cfg.ResolvPath)
	if err != nil {
		return err
	}

	firstIdx, firstIP := firstActiveNameserver(parsed)
	if firstIdx < 0 || firstIP == "" {
		return errors.New("no active nameserver line found in resolv.conf")
	}

	ok := dnsOK(firstIP, cfg)

	if ok {
		// Nothing to do: leave resolv.conf as-is.
		return nil
	}

	var out []string

	switch cfg.OnFail {
	case FailActionCommentOutFirst:
		out = make([]string, 0, len(lines))
		for i, l := range lines {
			if i == firstIdx {
				// Comment it out, preserving indentation.
				// Avoid double-commenting.
				trim := strings.TrimLeft(l, " \t")
				if strings.HasPrefix(trim, "#") {
					out = append(out, l)
				} else {
					ws := l[:len(l)-len(trim)]
					out = append(out, ws+"# "+trim)
				}
			} else {
				out = append(out, l)
			}
		}

	case FailActionReplaceNameservers:
		if len(cfg.ReplacementDNS) == 0 {
			return errors.New("OnFail=ReplaceNameservers but ReplacementDNS is empty")
		}

		// Keep everything except nameserver lines; then inject new nameserver lines at
		// the position of the first active nameserver (or first nameserver line if all commented).
		insertAt := firstIdx
		if insertAt < 0 {
			insertAt = 0
		}

		kept := make([]string, 0, len(lines))
		for _, p := range parsed {
			if p.isNS {
				continue
			}
			kept = append(kept, p.raw)
		}

		newNS := make([]string, 0, len(cfg.ReplacementDNS))
		for _, ip := range cfg.ReplacementDNS {
			ip = strings.TrimSpace(ip)
			if ip == "" {
				continue
			}
			newNS = append(newNS, "nameserver "+ip)
		}
		if len(newNS) == 0 {
			return errors.New("ReplacementDNS contained no usable entries")
		}

		// Insert deterministically.
		if insertAt > len(kept) {
			insertAt = len(kept)
		}
		out = append(out, kept[:insertAt]...)
		out = append(out, newNS...)
		out = append(out, kept[insertAt:]...)

	default:
		return fmt.Errorf("unknown OnFail action: %d", cfg.OnFail)
	}

	return writeFileAtomicIfChanged(cfg.ResolvPath, strings.Join(out, "\n")+"\n")
}

// resolved.conf: [Resolve] section, DNS= and FallbackDNS= space-separated IPs.
func runSystemd(cfg Config, resolvedConfPath string) error {
	content, err := os.ReadFile(resolvedConfPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", resolvedConfPath, err)
	}
	dnsIPs, fallbackIPs, rest, _, err := parseResolvedConf(string(content))
	if err != nil {
		return err
	}
	if len(dnsIPs) == 0 {
		return errors.New("no DNS= entries found in [Resolve] section")
	}
	firstIP := dnsIPs[0]
	if !dnsOK(firstIP, cfg) {
		switch cfg.OnFail {
		case FailActionCommentOutFirst:
			dnsIPs = dnsIPs[1:]
			if len(dnsIPs) == 0 && len(fallbackIPs) > 0 {
				dnsIPs = fallbackIPs
				fallbackIPs = nil
			}
		case FailActionReplaceNameservers:
			if len(cfg.ReplacementDNS) == 0 {
				return errors.New("on-fail=replace-nameservers but replacement-dns is empty")
			}
			dnsIPs = trimStrings(cfg.ReplacementDNS)
			if len(dnsIPs) == 0 {
				return errors.New("replacement-dns contained no usable entries")
			}
		default:
			return fmt.Errorf("unknown on-fail action: %d", cfg.OnFail)
		}
	}
	newContent := strings.Join(replaceResolvedRest(rest, dnsIPs, fallbackIPs), "\n") + "\n"
	return writeFileAtomicIfChanged(resolvedConfPath, newContent)
}

func parseResolvedConf(content string) (dns, fallback []string, rest []string, inResolve bool, err error) {
	lines := strings.Split(content, "\n")
	inResolve = false
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
				dns = parseResolvedDNSValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "DNS=")))
				rest = append(rest, line) // keep so replaceResolvedRest can replace it
				continue
			}
			if strings.HasPrefix(trimmed, "FallbackDNS=") {
				fallback = parseResolvedDNSValue(strings.TrimSpace(strings.TrimPrefix(trimmed, "FallbackDNS=")))
				rest = append(rest, line)
				continue
			}
		}
		rest = append(rest, line)
	}
	// rest still has original DNS= and FallbackDNS= lines; runSystemd will replace with new values
	return dns, fallback, rest, inResolve, nil
}

func parseResolvedDNSValue(v string) []string {
	if v == "" {
		return nil
	}
	// Values can be "1.2.3.4 2.3.4.5" or with optional :port %iface #sni - we only need the IP
	parts := strings.Fields(v)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// IPv6 with brackets [addr]:port
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

func replaceResolvedRest(rest []string, dns, fallback []string) []string {
	inResolve := false
	var out []string
	for _, line := range rest {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[Resolve]" {
			inResolve = true
			out = append(out, line)
			continue
		}
		if inResolve && (strings.HasPrefix(trimmed, "DNS=") || strings.HasPrefix(trimmed, "FallbackDNS=")) {
			if strings.HasPrefix(trimmed, "DNS=") {
				out = append(out, "DNS="+strings.Join(dns, " "))
			} else {
				out = append(out, "FallbackDNS="+strings.Join(fallback, " "))
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

func trimStrings(ss []string) []string {
	var out []string
	for _, s := range ss {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

const (
	nmBusName    = "org.freedesktop.NetworkManager"
	nmObjectPath = "/org/freedesktop/NetworkManager"
	nmSettingsPath = "/org/freedesktop/NetworkManager/Settings"
	nmSettingsInterface = "org.freedesktop.NetworkManager.Settings"
	nmSettingsConnInterface = "org.freedesktop.NetworkManager.Settings.Connection"
	nmConnectionActiveInterface = "org.freedesktop.NetworkManager.Connection.Active"
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
	dnsIPs, err := nmGetConnectionDNS(conn, settingsPath)
	if err != nil {
		return err
	}
	if len(dnsIPs) == 0 {
		return errors.New("connection has no IPv4 DNS servers configured")
	}
	firstIP := dnsIPs[0]
	if dnsOK(firstIP, cfg) {
		return nil
	}
	replacement := trimStrings(cfg.ReplacementDNS)
	if len(replacement) == 0 {
		return errors.New("on-fail=replace-nameservers but replacement-dns is empty")
	}
	if err := nmSetConnectionDNS(conn, settingsPath, replacement); err != nil {
		return err
	}
	if activePath != "" {
		return nmReactivateConnection(conn, settingsPath, activePath)
	}
	return nil
}

// nmGetActiveDNS returns DNS servers from the first active connection (for status command).
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
		// Get first active connection
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
	// Find connection by name (Id)
	obj := conn.Object(nmBusName, nmSettingsPath)
	var connectionPaths []dbus.ObjectPath
	if err := obj.Call(nmSettingsInterface+".GetConnections", 0).Store(&connectionPaths); err != nil {
		return "", "", fmt.Errorf("GetConnections: %w", err)
	}
	for _, p := range connectionPaths {
		id, _ := nmGetConnectionID(conn, p)
		if id == byName {
			// Find active path for this connection so we can re-activate
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
	// Prefer dns-data (array of strings); fallback to dns (array of uint32)
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
	ipv4["dns-data"] = dbus.MakeVariant(ips)
	delete(ipv4, "dns") // prefer dns-data
	obj := conn.Object(nmBusName, path)
	if err := obj.Call(nmSettingsConnInterface+".Update", 0, settings).Err; err != nil {
		return fmt.Errorf("Update connection: %w", err)
	}
	return nil
}

func nmReactivateConnection(conn *dbus.Conn, settingsPath, activePath dbus.ObjectPath) error {
	if activePath == "" {
		return errors.New("cannot re-activate: connection is not active (use default connection or activate it first)")
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

	// resolv.conf
	if _, parsed, err := readResolv(resolvPath); err == nil {
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

	// systemd-resolved: read dynamic resolv maintained by systemd-resolved (pure Go, no resolvectl)
	const systemdResolvPath = "/run/systemd/resolve/resolv.conf"
	if _, parsed, err := readResolv(systemdResolvPath); err == nil {
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

	// NetworkManager: get DNS from active connection via D-Bus (pure Go, no nmcli)
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
	if errors.As(err, &opErr) {
		return false
	}

	return true
}

func readResolv(path string) ([]string, []nsLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var lines []string
	var parsed []nsLine

	sc := bufio.NewScanner(f)
	i := 0
	for sc.Scan() {
		raw := sc.Text()
		lines = append(lines, raw)

		p := nsLine{idx: i, raw: raw}
		trimLeft := strings.TrimLeft(raw, " \t")
		p.leadingWS = raw[:len(raw)-len(trimLeft)]
		p.isComment = strings.HasPrefix(trimLeft, "#")

		trim := strings.TrimSpace(trimLeft)
		if strings.HasPrefix(trim, "#") {
			trim = strings.TrimSpace(strings.TrimPrefix(trim, "#"))
		}

		if strings.HasPrefix(trim, "nameserver") {
			fields := strings.Fields(trim)
			if len(fields) >= 2 {
				p.isNS = true
				p.nsIP = fields[1]
			}
		}

		parsed = append(parsed, p)
		i++
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}

	return lines, parsed, nil
}

func firstActiveNameserver(parsed []nsLine) (idx int, ip string) {
	for _, p := range parsed {
		if p.isNS && !p.isComment && p.nsIP != "" {
			return p.idx, p.nsIP
		}
	}
	return -1, ""
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
		tmp.Close()
		os.Remove(tmpName)
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
