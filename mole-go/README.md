# mole-go

A Go-based VPN client that wraps Hiddify-core for stable TUN mode support and easy configuration.

## Overview

mole-go is a command-line VPN client written in Go. It provides a user-friendly interface while leveraging Hiddify-core's robust TUN implementation for reliable VPN connectivity.

## Features

- 🚀 Simple YAML configuration
- 🌍 Automatic server IP geolocation display
- 📊 Real-time connection status
- 📝 Log viewing with follow mode
- 🔧 Built on proven Hiddify-core

## Installation

### Prerequisites

- Go 1.21 or later
- HiddifyCli installed on your system

### Build from source

```bash
git clone <repository>
cd mole-go
go build -o mole .
```

### Install HiddifyCli

```bash
# macOS
brew install hiddify

# Or download from GitHub releases
# https://github.com/hiddify/hiddify-core/releases
```

## Configuration

Create a configuration file at `~/.config/mole/config.yaml`:

```yaml
server: "vless://uuid@server.com:443?security=tls&sni=server.com&flow=xtls-rprx-vision"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
tun:
  enabled: true
  mtu: 1500
```

## Usage

### Start VPN connection

```bash
mole up
# or with specific config
mole up /path/to/config.yaml
```

### Stop VPN connection

```bash
mole down
```

### Check status

```bash
mole status
```

### View logs

```bash
# Show last 50 lines
mole logs

# Follow logs in real-time
mole logs -f

# Show last 100 lines
mole logs -n 100
```

### Configuration management

```bash
# Show current configuration
mole config show

# Validate configuration
mole config validate
```

## Project Structure

```
mole-go/
├── main.go              # Entry point
├── cmd/                 # CLI commands
│   ├── root.go          # Root command
│   ├── up.go            # Start VPN
│   ├── down.go          # Stop VPN
│   ├── status.go        # Show status
│   ├── logs.go          # View logs
│   └── config.go        # Config management
├── config/              # Configuration handling
│   ├── types.go         # Type definitions
│   └── parser.go        # Config parsing & conversion
├── core/                # Hiddify-core integration
│   └── hiddify.go       # Process management
└── utils/               # Utilities
    └── ipinfo.go        # IP geolocation
```

## Architecture

mole-go acts as a wrapper around Hiddify-core:

1. **Configuration Layer**: Parses user-friendly YAML config
2. **Conversion Layer**: Converts to Hiddify/sing-box JSON format
3. **Process Management**: Starts/stops HiddifyCli process
4. **Status Display**: Shows connection info and IP geolocation

## Comparison with Original mole (Rust)

| Feature | mole (Rust) | mole-go |
|---------|-------------|---------|
| Language | Rust | Go |
| TUN Implementation | Custom (problematic) | Hiddify-core (proven) |
| Binary Size | ~5MB | ~10MB |
| Maintenance | High | Low |
| Protocol Support | VLESS only | All Hiddify protocols |
| Stability | Issues | Stable |

## License

MIT
