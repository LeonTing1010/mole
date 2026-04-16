#!/bin/bash
# Build script for mole with proper macOS signing

set -e

echo "🔨 Building mole for macOS..."

# Build with explicit flags for macOS including UUID
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
    -ldflags="-s -w -buildmode=pie -linkmode=external" \
    -o ~/.mole/bin/mole \
    .

# Add UUID using dsymutil (creates dSYM with UUID)
echo "📝 Adding LC_UUID..."
dsymutil ~/.mole/bin/mole || true

# Add ad-hoc signature (required for macOS)
echo "🔏 Signing binary..."
codesign --sign - --force --preserve-metadata=entitlements,requirements,flags,runtime \
    ~/.mole/bin/mole

echo "✅ Build complete: ~/.mole/bin/mole"

# Alternative: strip and re-sign
# strip ~/.mole/bin/mole
# codesign --sign - --force ~/.mole/bin/mole

echo ""
echo "Testing..."
~/.mole/bin/mole --help || echo "Still failing, trying alternative..."
