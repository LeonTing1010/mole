#!/bin/bash
# Build script for mole with proper macOS signing

set -e

# Create bin directory
mkdir -p ~/.mole/bin

echo "🔨 Building mole for macOS..."

# Build locally first
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
    -ldflags="-s -w -buildmode=pie -linkmode=external" \
    -o mole \
    .

# Add UUID using dsymutil (creates dSYM with UUID)
echo "📝 Adding LC_UUID..."
dsymutil mole || true

# Add ad-hoc signature (required for macOS)
echo "🔏 Signing binary..."
codesign --sign - --force --preserve-metadata=entitlements,requirements,flags,runtime \
    mole

# Copy to ~/.mole/bin (overwrite existing)
echo "📦 Installing mole..."
cat mole > ~/.mole/bin/mole

# Re-sign the installed binary
echo "🔏 Re-signing binary..."
codesign --sign - --force --preserve-metadata=entitlements,requirements,flags,runtime \
    ~/.mole/bin/mole

# Create sing-box wrapper with environment variables
echo "📦 Creating sing-box wrapper..."
cat > ~/.mole/bin/sing-box << 'EOF'
#!/bin/bash

# Wrapper script to run sing-box with required environment variables

# Find the real sing-box binary (avoid finding ourselves)
if [ -f "/usr/local/bin/sing-box" ]; then
    REAL_SINGBOX="/usr/local/bin/sing-box"
elif [ -f "/usr/bin/sing-box" ]; then
    REAL_SINGBOX="/usr/bin/sing-box"
elif [ -f "/opt/sing-box/sing-box" ]; then
    REAL_SINGBOX="/opt/sing-box/sing-box"
else
    # Try to find in PATH, but exclude our wrapper
    REAL_SINGBOX=$(which -a sing-box 2>/dev/null | grep -v "/.mole/bin/sing-box" | head -1)
    if [ -z "$REAL_SINGBOX" ]; then
        echo "Error: sing-box binary not found" >&2
        exit 1
    fi
fi

# Run with environment variables
ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true "$REAL_SINGBOX" "$@"
EOF
chmod +x ~/.mole/bin/sing-box

echo "✅ Build complete: ~/.mole/bin/mole"