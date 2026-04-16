#!/bin/bash
# Mock HiddifyCli for testing

CONFIG_FILE=""

while [[ $# -gt 0 ]]; do
    case $1 in
        -c)
            CONFIG_FILE="$2"
            shift 2
            ;;
        run)
            shift
            ;;
        *)
            shift
            ;;
    esac
done

echo "🚀 Mock HiddifyCli started"
echo "📄 Config: $CONFIG_FILE"
echo "🌐 TUN interface: utun123"
echo "📊 Status: Running"

# Validate config exists
if [[ -n "$CONFIG_FILE" && ! -f "$CONFIG_FILE" ]]; then
    echo "❌ Config file not found: $CONFIG_FILE"
    exit 1
fi

# Simulate TUN interface creation
echo "🔧 Creating TUN interface..."
sleep 1
echo "✅ TUN interface created"

# Keep running until interrupted
trap 'echo "🛑 Shutting down..."; exit 0' SIGINT SIGTERM

while true; do
    sleep 1
done
