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

	"github.com/spf13/cobra"
)

var (
	appVersion = "0.1.3"
)

// Config is the common struct: filled from Cobra flag defaults, then overlaid with DNS/search from JSON.
type Config struct {
	ResolvPath     string
	Timeout        time.Duration
	Tries          int
	CheckDomain    string
	TunnelDNS      string   // from JSON
	StandardDNS    string   // from JSON
	TunnelSearch   []string // from JSON
	StandardSearch []string // from JSON
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
		timeout     time.Duration
		tries       int
		checkDomain string
		resolvPath  string
		configPath  string
	)
	rootCmd := &cobra.Command{
		Use:   "tunneldnsctl",
		Short: "Check tunnel DNS and update resolv.conf (tunnel first when up, standard when down)",
	}
	flags := rootCmd.PersistentFlags()
	flags.DurationVar(&timeout, "timeout", 400*time.Millisecond, "DNS check timeout per try")
	flags.IntVar(&tries, "tries", 2, "Number of DNS check attempts")
	flags.StringVar(&checkDomain, "check-domain", "internal.vodafoneinnovus.com", "Domain to resolve for reachability check")
	flags.StringVar(&configPath, "config", "", "Path to JSON config (tunnel_dns, standard_dns, tunnel_search, standard_search)")

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
			cfg, err := loadConfig(resolvPath, configPath, timeout, tries, checkDomain)
			if err != nil {
				return err
			}
			return runResolv(cfg)
		},
	}
	updateResolvCmd.Flags().StringVar(&resolvPath, "resolv-path", "/etc/resolv.conf", "Path to resolv.conf")

	updateCmd.AddCommand(updateResolvCmd)
	rootCmd.AddCommand(updateCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadConfig fills the common struct from flag defaults, then overlays DNS and search from the JSON file.
func loadConfig(resolvPath, configPath string, timeout time.Duration, tries int, checkDomain string) (Config, error) {
	cfg := Config{
		ResolvPath:  resolvPath,
		Timeout:     timeout,
		Tries:       tries,
		CheckDomain: checkDomain,
	}
	if configPath == "" {
		return cfg, errors.New("--config is required (path to JSON with tunnel_dns, standard_dns, tunnel_search, standard_search)")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return cfg, fmt.Errorf("reading config %s: %w", configPath, err)
	}
	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return cfg, fmt.Errorf("parsing config JSON: %w", err)
	}
	cfg.TunnelDNS = strings.TrimSpace(f.TunnelDNS)
	cfg.StandardDNS = strings.TrimSpace(f.StandardDNS)
	for _, s := range f.TunnelSearch {
		if t := strings.TrimSpace(s); t != "" {
			cfg.TunnelSearch = append(cfg.TunnelSearch, t)
		}
	}
	for _, s := range f.StandardSearch {
		if t := strings.TrimSpace(s); t != "" {
			cfg.StandardSearch = append(cfg.StandardSearch, t)
		}
	}
	if cfg.TunnelDNS == "" {
		return cfg, fmt.Errorf("config must set tunnel_dns")
	}
	if cfg.StandardDNS == "" {
		return cfg, fmt.Errorf("config must set standard_dns")
	}
	return cfg, nil
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
