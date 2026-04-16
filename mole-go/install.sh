#!/bin/bash
set -e

echo "🚀 Installing mole-go and sing-box..."

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

# Function to download sing-box from GitHub
download_singbox() {
    local VERSION="v1.11.4"
    local URL="https://github.com/SagerNet/sing-box/releases/download/${VERSION}/sing-box-${VERSION}-${OS}-${ARCH}.tar.gz"
    local TEMP_DIR=$(mktemp -d)
    
    echo -e "${BLUE}📥 Downloading sing-box ${VERSION}...${NC}"
    echo "   URL: $URL"
    
    if ! curl -L --fail --progress-bar -o "$TEMP_DIR/sing-box.tar.gz" "$URL" 2>&1; then
        echo -e "${RED}❌ Download failed${NC}"
        rm -rf "$TEMP_DIR"
        return 1
    fi
    
    # Check file size
    local FILE_SIZE=$(stat -f%z "$TEMP_DIR/sing-box.tar.gz" 2>/dev/null || stat -c%s "$TEMP_DIR/sing-box.tar.gz" 2>/dev/null || echo "0")
    if [ "$FILE_SIZE" -lt 1000 ]; then
        echo -e "${RED}❌ Downloaded file is too small (${FILE_SIZE} bytes)${NC}"
        rm -rf "$TEMP_DIR"
        return 1
    fi
    
    echo -e "${GREEN}✅ Downloaded ${FILE_SIZE} bytes${NC}"
    
    # Extract
    echo -e "${BLUE}📦 Extracting...${NC}"
    if ! tar -xzf "$TEMP_DIR/sing-box.tar.gz" -C "$TEMP_DIR" 2>&1; then
        echo -e "${RED}❌ Failed to extract archive${NC}"
        rm -rf "$TEMP_DIR"
        return 1
    fi
    
    # Find sing-box binary
    local SINGBOX_BIN=$(find "$TEMP_DIR" -name "sing-box" -type f 2>/dev/null | head -1)
    
    if [ -n "$SINGBOX_BIN" ]; then
        cp "$SINGBOX_BIN" "$BIN_DIR/sing-box"
        chmod +x "$BIN_DIR/sing-box"
        echo -e "${GREEN}✅ sing-box installed to $BIN_DIR/sing-box${NC}"
        rm -rf "$TEMP_DIR"
        return 0
    else
        echo -e "${RED}❌ sing-box binary not found in archive${NC}"
        echo "   Contents:"
        find "$TEMP_DIR" -type f | head -20
        rm -rf "$TEMP_DIR"
        return 1
    fi
}

# Check if sing-box already exists
if command -v sing-box &> /dev/null; then
    echo -e "${GREEN}✅ sing-box already installed: $(which sing-box)${NC}"
    sing-box version
elif [ -f "$BIN_DIR/sing-box" ]; then
    echo -e "${GREEN}✅ sing-box found in $BIN_DIR${NC}"
    "$BIN_DIR/sing-box" version
else
    echo -e "${YELLOW}📥 Installing sing-box...${NC}"
    
    if download_singbox; then
        echo -e "${GREEN}✅ sing-box installed successfully${NC}"
    else
        echo -e "${RED}❌ Failed to install sing-box${NC}"
        echo ""
        echo "Please install manually:"
        echo "  1. Visit: https://github.com/SagerNet/sing-box/releases"
        echo "  2. Download: sing-box-1.11.4-${OS}-${ARCH}.tar.gz"
        echo "  3. Extract and copy sing-box to $BIN_DIR/"
        exit 1
    fi
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
    ln -sf "$BIN_DIR/mole" /usr/local/bin/mole 2>/dev/null || true
    echo -e "${GREEN}✅ Created symlink: /usr/local/bin/mole${NC}"
else
    echo -e "${YELLOW}⚠️  Please add $BIN_DIR to your PATH:${NC}"
    echo "  export PATH=\"$BIN_DIR:\$PATH\""
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
echo ""
echo "Configuration: $MOLE_DIR/config.yaml"
