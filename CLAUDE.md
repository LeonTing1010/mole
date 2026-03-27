# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Mole is a Rust CLI tool that manages VPN/proxy connections via sing-box. Supports Hysteria2, VMess, VLESS, Trojan, and Shadowsocks protocols. Cross-platform (macOS + Linux). Requires sudo for TUN networking.

## Build & Test

```bash
cargo build --release          # Release build
cargo build                    # Debug build
cargo test                     # Run all unit tests (in uri.rs)
cargo test parse_hy2           # Run a single test by name
cargo run -- <subcommand>      # Run directly
```

## Architecture

**Data flow:** Proxy URIs (manual or subscription) → `uri.rs` parses into `ProxyNode` enum → `store.rs` persists to `~/.mole/nodes.json` → `config.rs` generates sing-box JSON (single node or multi-node strategy, with custom rules) → `runner.rs` manages the sing-box child process with auto-restart, health watchdog, and failover.

### Modules

- **`main.rs`** — CLI (clap derive) with commands: `add`, `up [-d] [--strategy]`, `down`, `ls`, `use`, `rm`, `bench`, `status`, `config`, `install`, `sub {add,update,ls,rm}`, `rule {add,ls,rm,clear}`, `service {install,uninstall}`. Handles Ctrl+C, panic hooks, daemon mode, failover loop.
- **`uri.rs`** — Multi-protocol proxy URI parser. Converts URI strings into `ProxyNode` variants and generates sing-box outbound JSON via `to_outbound()`. Largest module; contains all unit tests.
- **`config.rs`** — Generates complete sing-box JSON config. Supports: single-node mode, multi-node strategies (urltest/fallback/select), custom routing rules (domain/geoip/ip_cidr/etc.), bypass-cn with geo rule sets. Signature: `generate(nodes, custom_rules, bypass_cn, strategy)`.
- **`runner.rs`** — Downloads/installs sing-box binary, writes config, runs sing-box under sudo with auto-restart (exponential backoff, window reset after 5min stability), PID tracking, SIGTERM→SIGKILL graceful stop, structured logging to `~/.mole/mole.log`.
- **`store.rs`** — JSON persistence for nodes, subscriptions, rules, and strategy. Nodes track `source` (subscription name). Backward-compatible with v0.2 `nodes.json`.
- **`sub.rs`** — Subscription fetch + parse: base64-encoded multi-line URIs or plain text.
- **`status.rs`** — Connection monitoring: IP lookup via ipinfo.io, latency via ping, live speed via `NetMonitor`, daemon PID display.
- **`platform.rs`** — Cross-platform abstraction: OS/arch detection for sing-box downloads, `NetMonitor` (macOS: `netstat -I en0`, Linux: `/proc/net/dev`), default interface detection.
- **`bench.rs`** — Benchmarks all saved nodes sequentially (latency + download speed), auto-activates the fastest.

### Key design

- **Keepalive watchdog**: UDP DNS every 30s with response verification. 3 consecutive failures → set `HEALTH_KILL` flag → kill sing-box → retry loop handles restart.
- **Failover**: `ExitReason::MaxRetries` → `Store::next_node()` → try next untried node. Tracks `tried_nodes` to avoid loops.
- **Strategy mode**: All nodes become individual outbounds wrapped in a strategy outbound (urltest/fallback/select). sing-box handles internal switching.
- **Daemon mode**: `mole up -d` re-execs self with `MOLE_DAEMON=1` env, writes PID to `~/.mole/mole.pid`. `mole down` reads PID file + kills sing-box.
- **Service**: Generates `~/Library/LaunchAgents/com.mole.vpn.plist` (macOS) or `~/.config/systemd/user/mole.service` (Linux).

### Runtime paths

All state lives under `~/.mole/`: `bin/sing-box`, `nodes.json`, `config.json`, `sing-box.log`, `sing-box.prev.log`, `mole.log`, `mole.pid`, `daemon.log`, geo rule files.

## Key Implementation Notes

- sing-box runs as a sudo child process; all stop/cleanup paths must handle this
- `ProxyNode` enum variants carry protocol-specific fields; adding a protocol means updating the enum, `parse()`, `to_outbound()`, and `name()`
- `config::generate` takes a slice of `(&str, &ProxyNode)` pairs — tag and node. Single-node passes `[("proxy", &node)]`; strategy mode passes all nodes with sanitized name tags
- Custom rules are injected between `ip_is_private` and `bypass_cn` in route rules
- `store.rs` uses `#[serde(default)]` on new fields for backward compatibility with v0.2 data
- Platform-specific code is isolated in `platform.rs` via `#[cfg(target_os)]` modules
