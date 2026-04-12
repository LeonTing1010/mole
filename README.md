# mole

Multi-protocol TUN VPN client for macOS and Linux, powered by [sing-box](https://github.com/SagerNet/sing-box).

```
  .--.
 / oo \    mole v0.4.0
( >  < )   TUN VPN powered by sing-box
  '--'
```

## Features

- **8 protocols** — Hysteria2, VMess, VLESS (Reality), Trojan, Shadowsocks, TUIC, WireGuard, Hysteria
- **Subscriptions** — Import nodes from provider URLs, batch update
- **Auto-discover** — Find working nodes from configurable sources, benchmark speed, keep the fastest
- **IPv6 detection** — Identify IPv6-capable nodes in bench and ls output
- **Smart routing** — Custom rules (domain, geoip, ip_cidr), bypass-cn mode
- **Multi-node strategies** — urltest (auto lowest latency), fallback, select
- **Speed benchmark** — Real download speed test (KB/s), 8x parallel, auto-activate fastest
- **Auto-recovery** — Health watchdog, exponential backoff restart, node failover
- **Daemon mode** — Background operation with `mole up -d`
- **System service** — launchd (macOS) / systemd (Linux) integration
- **Live monitoring** — Real-time speed, IP, latency display

## Install

### From source (requires Rust)

```bash
cargo install --git https://github.com/LeonTing1010/mole.git
```

### From binary release

Download from [Releases](https://github.com/LeonTing1010/mole/releases):

```bash
# macOS (Apple Silicon)
curl -LO https://github.com/LeonTing1010/mole/releases/latest/download/mole-darwin-arm64.tar.gz
tar xzf mole-darwin-arm64.tar.gz
sudo mv mole /usr/local/bin/

# macOS (Intel)
curl -LO https://github.com/LeonTing1010/mole/releases/latest/download/mole-darwin-amd64.tar.gz
tar xzf mole-darwin-amd64.tar.gz
sudo mv mole /usr/local/bin/

# Linux (x86_64)
curl -LO https://github.com/LeonTing1010/mole/releases/latest/download/mole-linux-amd64.tar.gz
tar xzf mole-linux-amd64.tar.gz
sudo mv mole /usr/local/bin/
```

### Homebrew (macOS)

```bash
brew install LeonTing1010/tap/mole
```

## Quick Start

```bash
# Install sing-box
mole install

# Add a proxy node (use -t to test connectivity and detect IPv6)
mole add "hysteria2://password@server:443?sni=example.com#MyNode" -t

# Connect
mole up

# Disconnect
mole down
```

## Auto-Discover Nodes

Find working proxy nodes automatically from configurable sources:

```bash
# Discover, benchmark, and activate the fastest nodes
mole sub discover

# Only keep IPv6-capable nodes
mole sub discover --v6
```

On first run, default sources are fetched from the mole repo. You can manage sources manually:

```bash
mole sub source add <url> --name <name>          # Subscription URL
mole sub source add <url> --name <name> --html   # HTML page (extracts proxy URIs)
mole sub source ls                                # List configured sources
mole sub source rm <name>                         # Remove a source
```

For date-based sources (e.g. daily-updated nodes), edit `~/.mole/sources.json` directly:

```json
{
  "name": "daily-nodes",
  "url": "https://example.com/uploads/{YYYY}/{MM}/{N}-{YYYYMMDD}.txt",
  "source_type": "date-pattern",
  "count": 5
}
```

Placeholders `{YYYY}`, `{MM}`, `{DD}`, `{YYYYMMDD}`, `{N}` are auto-filled with today's date. Falls back to previous days if today's files aren't available yet.

## Deploy Your Own Server

Don't have a proxy server? Set one up in 60 seconds.

**1. Get a VPS** (if you don't have one):

- [Vultr](https://www.vultr.com/?ref=9893285) — from $2.50/mo, 32 locations

**2. Run the setup script** on your VPS:

```bash
curl -fsSL https://leonting1010.github.io/mole/install.sh | sudo bash
```

This installs sing-box, configures VLESS + Reality, and prints a URI.

**3. Connect** from your local machine:

```bash
mole add "vless://...the-uri-from-step-2..." -t
mole up
```

**Customize** with environment variables:

```bash
MOLE_PORT=8443 MOLE_SNI=www.apple.com curl -fsSL ... | sudo bash
```

## Usage

### Node Management

```bash
mole add <uri> [-t] [--name custom]  # Add a node (-t to test + detect IPv6)
mole ls                              # List all nodes (shows v4/v6 tags)
mole use <name|index>                # Switch active node
mole rm <name>                       # Remove a node
mole bench                           # Benchmark speed, show v4/v6, activate fastest
mole bench --clean                   # Same + remove failed nodes
```

### Subscriptions

```bash
mole sub add <url> [--name provider]   # Import nodes from subscription URL
mole sub update [--test]               # Refresh all subscriptions
mole sub ls                            # List subscriptions
mole sub rm <name>                     # Remove subscription + its nodes
mole sub discover [--v6]               # Auto-find nodes from configured sources
```

### Connect

```bash
mole up                          # Connect (foreground)
mole up -d                       # Connect (background daemon)
mole up --strategy urltest       # Auto-select lowest latency node
mole up --strategy fallback      # Use first available node
mole up --bypass-cn false        # Global mode (no China bypass)
mole down                        # Disconnect
mole status                      # Show connection info
```

### Routing Rules

```bash
mole rule add domain example.com direct        # Direct access for domain
mole rule add domain_suffix .cn direct         # Direct for all .cn domains
mole rule add domain_keyword ads block         # Block ad domains
mole rule add geoip jp proxy                   # Route Japan IP through proxy
mole rule add ip_cidr 10.0.0.0/8 direct       # Direct for private network
mole rule ls                                   # List all rules
mole rule rm <index>                           # Remove a rule
mole rule clear                                # Remove all rules
```

### System Service

```bash
mole service install     # Auto-start at login (launchd/systemd)
mole service uninstall   # Remove auto-start
```

### Other

```bash
mole install             # Download sing-box binary
mole config              # Print generated sing-box config (dry run)
mole test [--all]        # Test node connectivity (no sudo)
mole qr [name]           # Generate QR code for a node
mole rename              # Normalize all node names
```

## How It Works

```
URI / Subscription / Discover
       |
   uri.rs (parse 8 protocols)
       |
   store.rs (persist to ~/.mole/nodes.json)
       |
   config.rs (generate sing-box JSON)
       |
   runner.rs (manage sing-box process)
       |
   sing-box (TUN networking via sudo)
```

mole is a thin orchestration layer. All actual networking is handled by sing-box. mole translates proxy URIs into sing-box configs and manages the sing-box lifecycle with auto-restart, health monitoring, and failover.

### Data directory

All state lives under `~/.mole/`:

```
~/.mole/
  bin/sing-box          # sing-box binary (auto-installed)
  nodes.json            # nodes, subscriptions, rules
  sources.json          # discover source configuration
  bench.json            # benchmark results (speed + IPv6)
  config.json           # current sing-box config
  mole.log              # mole structured log
  sing-box.log          # sing-box output
```

## Requirements

- macOS 11+ or Linux (kernel 4.1+ with TUN support)
- sudo access (required for TUN interface)

## License

MIT
