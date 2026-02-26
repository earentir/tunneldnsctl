# tunneldnsctl

Check tunnel DNS reachability and update DNS configuration: resolv.conf, systemd-resolved, or NetworkManager. When the tunnel DNS is up it is placed first (with its search domains); when it is down it is removed and the standard DNS is used if no other active nameserver exists. A **status** command reports current DNS from resolv.conf, systemd-resolved, and NetworkManager.

## Requirements

- **tunnel_dns** and **standard_dns** must be set, either via flags or via a config file.

## Options (flags)

Every option has a flag. Flags are the defaults; values from a config file (when `--config` is used) override the corresponding flag values.

| Flag | Default | Description |
|------|---------|-------------|
| `--timeout` | `400ms` | DNS check timeout per try |
| `--tries` | `2` | Number of DNS check attempts |
| `--check-domain` | `internal.vodafoneinnovus.com` | Domain to resolve for reachability |
| `--resolv-path` | `/etc/resolv.conf` | Path to resolv.conf (used by update resolv and by status) |
| `--config` | (none) | Path to JSON config file (optional) |
| `--tunnel-dns` | (none) | Tunnel DNS server IP |
| `--standard-dns` | (none) | Standard DNS server IP |
| `--tunnel-search` | (none) | Tunnel search domains (can be repeated) |
| `--standard-search` | (none) | Standard search domains (can be repeated) |

**update systemd:** `--resolved-conf` (default `/etc/systemd/resolved.conf`) — Path to systemd resolved.conf.

**update nm:** `--connection` (default: first active connection) — NetworkManager connection name to update.

**status:** `--resolv-path` (default `/etc/resolv.conf`) — Path to resolv.conf to show for the "resolv.conf" source.

Global:

- `--version`, `-v` — Print version and exit.

---

## Usage

### resolv.conf (update resolv)

Update the system `resolv.conf` (or the file at `--resolv-path`).

**Behaviour:**

- **Tunnel DNS reachable:** Tunnel DNS is written as the first nameserver; other active nameservers follow (each once). Search line: tunnel search domains then standard.
- **Tunnel DNS not reachable:** Tunnel DNS is removed. If there is at least one other active nameserver, nothing else is added. If there is none, the standard DNS is added. Search line: standard search domains only.

### systemd-resolved (update systemd)

Update systemd-resolved’s configuration file (typically `/etc/systemd/resolved.conf` or a drop-in). Uses the same tunnel/standard logic as resolv: tunnel up → `DNS=` tunnel, `FallbackDNS=` standard, `Domains=` tunnel + standard search; tunnel down → `DNS=` standard (or current list with tunnel removed), `Domains=` standard search only.

**Flags:** Same as above; add `--resolved-conf` to point to the resolved.conf file (default `/etc/systemd/resolved.conf`).

```bash
./tunneldnsctl update systemd --config config.example.json
./tunneldnsctl update systemd --resolved-conf /etc/systemd/resolved.conf --config config.example.json
```

### NetworkManager (update nm)

Update the DNS of a NetworkManager connection via D-Bus. Same tunnel/standard logic: tunnel up → DNS list is tunnel first, then standard, then others (deduped); tunnel down → tunnel removed, or standard only if no other DNS.

**Flags:** Same as above; add `--connection` to choose the connection by name (default: first active connection).

```bash
./tunneldnsctl update nm --config config.example.json
./tunneldnsctl update nm --connection "My Ethernet" --config config.example.json
```

### status (read-only)

Report which DNS servers are in use for each method: resolv.conf, systemd-resolved (from `/run/systemd/resolve/resolv.conf`), and NetworkManager (first active connection). Does not require `--config` or tunnel/standard flags.

```bash
./tunneldnsctl status
./tunneldnsctl status --resolv-path /etc/resolv.conf
```

Example output:

```
resolv.conf:        192.168.5.5
systemd-resolved:   127.0.0.53
NetworkManager:     192.168.1.1
```

---

### Examples with flags only

Run using only flags (no config file). You must set at least `--tunnel-dns` and `--standard-dns`.

```bash
# Minimal: tunnel and standard DNS only
./tunneldnsctl update resolv \
  --tunnel-dns 192.168.5.5 \
  --standard-dns 192.168.178.22

# With search domains (repeat flag for multiple)
./tunneldnsctl update resolv \
  --tunnel-dns 192.168.5.5 \
  --standard-dns 192.168.178.22 \
  --tunnel-search internal.example.com \
  --tunnel-search corp.example.com \
  --standard-search local

# Custom resolv path and check domain
./tunneldnsctl update resolv \
  --resolv-path /etc/resolv.conf \
  --check-domain internal.mycompany.com \
  --tunnel-dns 192.168.5.5 \
  --standard-dns 192.168.178.22
```

---

### Examples with config file

Put DNS and search entries in a JSON file and pass it with `--config`. Flags still apply as defaults; the config file overrides tunnel/standard DNS and search.

**Config file schema:**

- `tunnel_dns` (string) — Tunnel DNS server IP
- `standard_dns` (string) — Standard DNS server IP
- `tunnel_search` (array of strings) — Tunnel search domains
- `standard_search` (array of strings) — Standard search domains

Example `config.example.json`:

```json
{
  "tunnel_dns": "192.168.5.5",
  "standard_dns": "192.168.178.22",
  "tunnel_search": ["internal.example.com"],
  "standard_search": ["local"]
}
```

**Run with config only:**

```bash
./tunneldnsctl update resolv --config config.example.json
```

**Run with config and flag overrides:**

```bash
# Override resolv path
./tunneldnsctl update resolv --config config.example.json --resolv-path /tmp/resolv.conf

# Override timeout and check domain; DNS/search from config
./tunneldnsctl update resolv --config config.example.json \
  --timeout 500ms \
  --check-domain internal.mycompany.com
```

---

### Combined: flags as defaults, config to override

Flags are always the base. If you set both a flag and a value in the config file, the config file wins for that field.

```bash
# Default tunnel/standard from config; override standard-dns on the command line
./tunneldnsctl update resolv --config config.example.json --standard-dns 10.0.0.1
```

---

## Commands

| Command | Description |
|---------|-------------|
| `update resolv` | Check tunnel DNS and update resolv.conf |
| `update systemd` | Check tunnel DNS and update systemd-resolved (resolved.conf) |
| `update nm` | Check tunnel DNS and update NetworkManager connection DNS |
| `status` | Report DNS servers from resolv.conf, systemd-resolved, and NetworkManager (read-only) |

---

## Building

```bash
go build -o tunneldnsctl .
```

## Version

```bash
./tunneldnsctl --version
```
