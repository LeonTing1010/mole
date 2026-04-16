# mole

A simple command-line VPN client for macOS/Linux, powered by Hiddify-core.

## Overview

mole is a lightweight VPN client that wraps [Hiddify-core](https://github.com/hiddify/hiddify-core) for stable TUN mode support. It provides a user-friendly CLI interface while leveraging Hiddify's robust networking stack.

**Note:** This project has been rewritten in Go. The original Rust version is archived in `archive/rust-version/`.

## Features

- 🚀 Simple YAML configuration
- 🌍 Automatic server IP geolocation display
- 📊 Real-time connection status
- 📝 Log viewing with follow mode
- 🔧 Built on proven Hiddify-core
- 🎯 Support for VLESS, VMess, Trojan protocols

## Quick Start

### Installation

```bash
# Clone the repository
git clone <repository-url>
cd mole/mole-go

# Run install script
chmod +x install.sh
./install.sh
```

Or manually:

```bash
# Install HiddifyCli
bash <(curl -fsSL https://i.hiddify.com)

# Build mole
cd mole-go
go build -o ~/.mole/bin/mole .

# Create config
mkdir -p ~/.mole
cat > ~/.mole/config.yaml << 'EOF'
server: "vless://your-uuid@your-server.com:443?security=tls&sni=your-server.com"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
EOF
```

### Usage

```bash
# Start VPN
mole up

# Stop VPN
mole down

# Check status
mole status

# View logs
mole logs -f

# Validate config
mole config validate
```

## Configuration

Edit `~/.mole/config.yaml`:

```yaml
# Server URL (VLESS/VMess/Trojan)
server: "vless://uuid@server.com:443?security=tls&sni=server.com"

# DNS servers
dns:
  - "1.1.1.1"
  - "8.8.8.8"

# Log level: debug, info, warn, error
log_level: "info"

# TUN settings
tun:
  enabled: true
  mtu: 1500
```

## Project Structure

```
mole/
├── mole-go/              # Go implementation (current)
│   ├── cmd/              # CLI commands
│   ├── config/           # Configuration parsing
│   ├── core/             # Hiddify integration
│   ├── utils/            # Utilities
│   ├── install.sh        # Install script
│   ├── Makefile          # Build automation
│   └── README.md         # Documentation
│
├── archive/
│   └── rust-version/     # Original Rust implementation
│
└── docs/                 # Additional documentation
```

## Requirements

- macOS or Linux
- Go 1.21+ (for building)
- HiddifyCli (auto-installed by install script)

## Migration from Rust Version

If you were using the old Rust version:

1. Configuration location remains the same: `~/.mole/config.yaml`
2. Logs are still at: `~/.mole/mole.log`
3. Simply run the new install script to upgrade

## Development

```bash
cd mole-go

# Build
make build

# Install locally
make install

# Run tests
make test

# Clean
make clean
```

## License

MIT License - see [LICENSE](LICENSE) file

## Acknowledgments

- [Hiddify](https://hiddify.com/) - For the excellent Hiddify-core
- [sing-box](https://sing-box.sagernet.org/) - The underlying proxy framework
