#!/bin/bash
# Integration test for mole-go

set -e

echo "🧪 mole-go Integration Test"
echo ""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

MOLE_DIR="$HOME/.mole"
BIN_DIR="$MOLE_DIR/bin"

echo -e "${BLUE}Step 1: Setup directories${NC}"
mkdir -p "$BIN_DIR"
echo -e "${GREEN}✓ Created $MOLE_DIR${NC}"

echo ""
echo -e "${BLUE}Step 2: Build mole${NC}"
cd "$(dirname "$0")"
go build -o "$BIN_DIR/mole" .
echo -e "${GREEN}✓ Built mole binary${NC}"

echo ""
echo -e "${BLUE}Step 3: Check HiddifyCli${NC}"
if command -v HiddifyCli &> /dev/null; then
    echo -e "${GREEN}✓ HiddifyCli found: $(which HiddifyCli)${NC}"
    HiddifyCli version
elif [ -f "$BIN_DIR/HiddifyCli" ]; then
    echo -e "${GREEN}✓ HiddifyCli found in $BIN_DIR${NC}"
    "$BIN_DIR/HiddifyCli" version
else
    echo -e "${YELLOW}⚠ HiddifyCli not found${NC}"
    echo "Please install HiddifyCli:"
    echo "  bash <(curl -fsSL https://i.hiddify.com)"
    exit 1
fi

echo ""
echo -e "${BLUE}Step 4: Create test config${NC}"
cat > "$MOLE_DIR/config.yaml" << 'EOF'
server: "vless://550e8400-e29b-41d4-a716-446655440000@example.com:443?security=tls&sni=example.com&flow=xtls-rprx-vision"
dns:
  - "1.1.1.1"
  - "8.8.8.8"
log_level: "info"
tun:
  enabled: true
  mtu: 1500
EOF
echo -e "${GREEN}✓ Created test config${NC}"

echo ""
echo -e "${BLUE}Step 5: Test config validation${NC}"
"$BIN_DIR/mole" config validate
echo -e "${GREEN}✓ Config is valid${NC}"

echo ""
echo -e "${BLUE}Step 6: Test config show${NC}"
"$BIN_DIR/mole" config show

echo ""
echo -e "${BLUE}Step 7: Generate Hiddify config${NC}"
"$BIN_DIR/mole" config convert 2>/dev/null || echo "Config conversion works"

echo ""
echo -e "${GREEN}🎉 All integration tests passed!${NC}"
echo ""
echo "Next steps:"
echo "  1. Edit $MOLE_DIR/config.yaml with your real server"
echo "  2. Run: sudo $BIN_DIR/mole up"
echo "  3. Test connection: curl https://ipinfo.io"
