#!/bin/bash
set -e

# EAM Collector installer
# Usage: ./install.sh [config.yaml path]

BINARY_NAME="eam-collector"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="$HOME/.eam-collector"

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
esac

BINARY="${BINARY_NAME}-${OS}-${ARCH}"

echo "══════════════════════════════════════════"
echo "  EAM Collector Installer"
echo "══════════════════════════════════════════"
echo "  Platform: ${OS}/${ARCH}"
echo ""

# Check if binary exists in current dir (local install) or download
if [ -f "dist/${BINARY}" ]; then
    echo "Installing from local build..."
    cp "dist/${BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"
elif [ -f "${BINARY}" ]; then
    cp "${BINARY}" "${INSTALL_DIR}/${BINARY_NAME}"
else
    echo "Binary not found. Run 'make build-all' first, or place ${BINARY} in this directory."
    exit 1
fi

chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
echo "  Binary installed to ${INSTALL_DIR}/${BINARY_NAME}"

# Set up config directory
mkdir -p "${CONFIG_DIR}"
if [ ! -f "${CONFIG_DIR}/config.yaml" ]; then
    if [ -n "$1" ] && [ -f "$1" ]; then
        cp "$1" "${CONFIG_DIR}/config.yaml"
        echo "  Config copied from $1"
    else
        cp config.example.yaml "${CONFIG_DIR}/config.yaml" 2>/dev/null || true
        echo "  Config template created at ${CONFIG_DIR}/config.yaml"
        echo "  >>> Edit ${CONFIG_DIR}/config.yaml with your server URL and API key <<<"
    fi
fi

# Install daemon
if [ "$OS" = "darwin" ]; then
    PLIST_SRC="install/com.eam.collector.plist"
    PLIST_DST="$HOME/Library/LaunchAgents/com.eam.collector.plist"

    # Stop existing daemon if running
    launchctl unload "$PLIST_DST" 2>/dev/null || true

    cp "$PLIST_SRC" "$PLIST_DST"
    launchctl load "$PLIST_DST"
    echo "  Daemon installed (launchd)"
    echo "  Logs: /tmp/eam-collector.log"

elif [ "$OS" = "linux" ]; then
    SERVICE_SRC="install/eam-collector.service"
    SERVICE_DST="/etc/systemd/system/eam-collector.service"

    if [ -d /etc/systemd/system ]; then
        sudo cp "$SERVICE_SRC" "$SERVICE_DST"
        sudo systemctl daemon-reload
        sudo systemctl enable eam-collector
        sudo systemctl start eam-collector
        echo "  Daemon installed (systemd)"
        echo "  Logs: journalctl -u eam-collector -f"
    fi
fi

echo ""
echo "  Installation complete."
echo ""
