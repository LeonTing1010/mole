# mole

Multi-protocol TUN VPN client for macOS and Linux, powered by [sing-box](https://github.com/SagerNet/sing-box).

```
  .--.
 / oo \    mole v0.3.0
( >  < )   TUN VPN powered by sing-box
  '--'
```

## Features

- **5 protocols** — Hysteria2, VMess, VLESS (Reality), Trojan, Shadowsocks
- **Subscriptions** — Import nodes from provider URLs, batch update
- **Smart routing** — Custom rules (domain, geoip, ip_cidr), bypass-cn mode
- **Multi-node strategies** — urltest (auto lowest latency), fallback, select
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
# Add a proxy node
mole add "hysteria2://password@server:443?sni=example.com#MyNode"

# Connect
mole up

# Disconnect
mole down
```

## Deploy Your Own Server

Don't have a proxy server? Set one up in 60 seconds.

**1. Get a VPS** (if you don't have one):

- [Vultr](https://www.vultr.com/?ref=9893285) — from $2.50/mo, 32 locations

**2. Run the setup script** on your VPS:

```bash
curl -fsSL https://raw.githubusercontent.com/LeonTing1010/mole/master/scripts/mole-server.sh | sudo bash
```

This installs sing-box, configures VLESS + Reality, and prints a URI.

**3. Connect** from your local machine:

```bash
mole add "vless://...the-uri-from-step-2..."
mole up
```

**Customize** with environment variables:

```bash
MOLE_PORT=8443 MOLE_SNI=www.apple.com curl -fsSL ... | sudo bash
```

## Usage

### Node Management

```bash
mole add <uri> [--name custom]   # Add a node (auto-activates)
mole ls                          # List all nodes
mole use <name>                  # Switch active node
mole rm <name>                   # Remove a node
mole bench                       # Test all nodes, activate fastest
```

### Subscriptions

```bash
mole sub add <url> [--name provider]   # Import nodes from subscription URL
mole sub update                        # Refresh all subscriptions
mole sub ls                            # List subscriptions
mole sub rm <name>                     # Remove subscription + its nodes
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
```

## How It Works

```
URI / Subscription
       |
   uri.rs (parse)
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
  config.json           # current sing-box config
  mole.log              # mole structured log
  sing-box.log          # sing-box output
```

## Requirements

- macOS 11+ or Linux (kernel 4.1+ with TUN support)
- sudo access (required for TUN interface)

## License

MIT
