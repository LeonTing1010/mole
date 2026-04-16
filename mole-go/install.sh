#!/bin/bash
set -e

echo "🚀 Installing mole-go and HiddifyCli..."

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Detect architecture
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

case "$ARCH" in
    x86_64)
        ARCH="amd64"
        ;;
    arm64|aarch64)
        ARCH="arm64"
        ;;
    *)
        echo -e "${RED}Unsupported architecture: $ARCH${NC}"
        exit 1
        ;;
esac

echo -e "${BLUE}📦 Detected: $OS-$ARCH${NC}"

# Create mole directory
MOLE_DIR="$HOME/.mole"
BIN_DIR="$MOLE_DIR/bin"
mkdir -p "$BIN_DIR"
echo -e "${BLUE}📁 Created directory: $MOLE_DIR${NC}"

# Check if HiddifyCli already exists
if command -v HiddifyCli &> /dev/null; then
    echo -e "${GREEN}✅ HiddifyCli already installed${NC}"
    HiddifyCli version
else
    echo -e "${YELLOW}📥 Installing HiddifyCli via official script...${NC}"
    
    # Official Hiddify install script
    bash <(curl -fsSL https://i.hiddify.com) || {
        echo -e "${RED}❌ Failed to install HiddifyCli${NC}"
        echo "Please check your internet connection or install manually:"
        echo "  bash <(curl -fsSL https://i.hiddify.com)"
        exit 1
    }
    
    echo -e "${GREEN}✅ HiddifyCli installed successfully${NC}"
fi

# Build mole-go
echo -e "${BLUE}🔨 Building mole-go...${NC}"
if command -v go &> /dev/null; then
    cd "$(dirname "$0")"
    go build -ldflags="-s -w" -o "$BIN_DIR/mole" .
    echo -e "${GREEN}✅ mole-go built successfully${NC}"
else
    echo -e "${RED}❌ Go not found. Please install Go 1.21+${NC}"
    echo "Visit: https://go.dev/dl/"
    exit 1
fi

# Create symlink to PATH
if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
    ln -sf "$BIN_DIR/mole" /usr/local/bin/mole
    echo -e "${GREEN}✅ Created symlink: /usr/local/bin/mole${NC}"
else
    echo -e "${YELLOW}⚠️  Please add $BIN_DIR to your PATH:${NC}"
    echo "  export PATH=\"$BIN_DIR:\$PATH\""
    echo ""
    echo "Add to your ~/.zshrc or ~/.bashrc:"
    echo "  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc"
fi

# Create sample config if not exists
if [ ! -f "$MOLE_DIR/config.yaml" ]; then
    cat > "$MOLE_DIR/config.yaml" << 'EOF'
# mole configuration file
# Replace with your actual VLESS/Vmess/Trojan server URL
server: "vless://uuid@your-server.com:443?security=tls&sni=your-server.com&flow=xtls-rprx-vision"

# DNS servers
dns:
  - "1.1.1.1"
  - "8.8.8.8"

# Log level: debug, info, warn, error
log_level: "info"

# TUN interface settings
tun:
  enabled: true
  mtu: 1500
EOF
    echo -e "${YELLOW}⚠️  Please edit $MOLE_DIR/config.yaml with your server details${NC}"
fi

echo ""
echo -e "${GREEN}🎉 Installation complete!${NC}"
echo ""
echo "Usage:"
echo "  mole up              # Start VPN"
echo "  mole down            # Stop VPN"
echo "  mole status          # Check status"
echo "  mole logs -f         # View logs"
echo "  mole config validate # Validate config"
echo ""
echo "Configuration: $MOLE_DIR/config.yaml"
echo "Logs: $MOLE_DIR/mole.log"
echo ""
echo -e "${BLUE}Quick start:${NC}"
echo "  1. Edit config: vim $MOLE_DIR/config.yaml"
echo "  2. Start VPN:   mole up"
echo "  3. Check status: mole status"
