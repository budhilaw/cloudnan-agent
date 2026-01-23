#!/bin/bash
# Cloudnan Agent Installation Script
# Usage: curl -sSL https://cloudnan.com/downloads/install.sh | sudo bash -s -- --token TOKEN --id ID --panel PANEL_URL
# Uninstall: curl -sSL https://cloudnan.com/downloads/install.sh | sudo bash -s -- --uninstall

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
AGENT_TOKEN=""
AGENT_ID=""
PANEL_URL=""
UNINSTALL=false
UPGRADE=false
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/cloudnan"
PKI_DIR="/etc/cloudnan/pki"
LOG_FILE="/var/log/cloudnan-agent.log"
SERVICE_NAME="cloudnan-agent"
BINARY_URL=""

# Print banner
print_banner() {
    echo -e "${BLUE}"
    echo "   _____ _                 _                   "
    echo "  / ____| |               | |                  "
    echo " | |    | | ___  _   _  __| |_ __   __ _ _ __  "
    echo " | |    | |/ _ \| | | |/ _\` | '_ \ / _\` | '_ \ "
    echo " | |____| | (_) | |_| | (_| | | | | (_| | | | |"
    echo "  \_____|_|\___/ \__,_|\__,_|_| |_|\__,_|_| |_|"
    echo ""
    echo -e "${NC}"
    echo -e "${GREEN}Agent Installation Script (with mTLS)${NC}"
    echo ""
}

# Print step
print_step() {
    echo -e "${BLUE}[*]${NC} $1"
}

# Print success
print_success() {
    echo -e "${GREEN}[✓]${NC} $1"
}

# Print error
print_error() {
    echo -e "${RED}[✗]${NC} $1"
}

# Print warning
print_warning() {
    echo -e "${YELLOW}[!]${NC} $1"
}

# Parse arguments
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --token)
                AGENT_TOKEN="$2"
                shift 2
                ;;
            --id)
                AGENT_ID="$2"
                shift 2
                ;;
            --panel)
                PANEL_URL="$2"
                shift 2
                ;;
            --uninstall)
                UNINSTALL=true
                shift
                ;;
            --upgrade)
                UPGRADE=true
                shift
                ;;
            --help)
                echo "Usage: $0 --token TOKEN --id ID --panel PANEL_URL"
                echo "       $0 --uninstall"
                echo "       $0 --upgrade"
                echo ""
                echo "Options:"
                echo "  --token     Authentication token (required for install)"
                echo "  --id        Agent ID (required for install)"
                echo "  --panel     Panel URL (required for install)"
                echo "              Example: https://panel.example.com"
                echo "  --uninstall Remove agent and all configuration"
                echo "  --upgrade   Upgrade existing agent to latest version (OTA update)"
                exit 0
                ;;
            *)
                print_error "Unknown option: $1"
                exit 1
                ;;
        esac
    done
}

# Validate required arguments for install
validate_args() {
    if [ -z "$AGENT_TOKEN" ]; then
        print_error "Missing required argument: --token"
        exit 1
    fi
    if [ -z "$AGENT_ID" ]; then
        print_error "Missing required argument: --id"
        exit 1
    fi
    if [ -z "$PANEL_URL" ]; then
        print_error "Missing required argument: --panel"
        exit 1
    fi
}

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        print_error "This script must be run as root (use sudo)"
        exit 1
    fi
}

# Detect architecture
detect_arch() {
    ARCH=$(uname -m)
    case $ARCH in
        x86_64)
            BINARY_URL="https://cloudnan.com/downloads/cloudnan-agent-linux-amd64"
            ;;
        aarch64|arm64)
            BINARY_URL="https://cloudnan.com/downloads/cloudnan-agent-linux-arm64"
            ;;
        *)
            print_error "Unsupported architecture: $ARCH"
            exit 1
            ;;
    esac
    print_step "Detected architecture: $ARCH"
}

# Download agent binary
download_agent() {
    print_step "Downloading agent binary..."
    
    # Create install directory
    mkdir -p "$INSTALL_DIR"
    
    # Download binary
    if command -v curl &> /dev/null; then
        curl -sSL "$BINARY_URL" -o "$INSTALL_DIR/$SERVICE_NAME"
    elif command -v wget &> /dev/null; then
        wget -q "$BINARY_URL" -O "$INSTALL_DIR/$SERVICE_NAME"
    else
        print_error "Neither curl nor wget found. Please install one of them."
        exit 1
    fi
    
    # Make executable
    chmod +x "$INSTALL_DIR/$SERVICE_NAME"
    
    print_success "Agent downloaded to $INSTALL_DIR/$SERVICE_NAME"
}

# Create config directory with proper permissions
create_config_dir() {
    print_step "Creating configuration directories..."
    mkdir -p "$CONFIG_DIR"
    mkdir -p "$PKI_DIR"
    chmod 700 "$PKI_DIR"
    print_success "Configuration directories created"
}

# Create systemd service
create_systemd_service() {
    print_step "Creating systemd service..."
    
    cat > "/etc/systemd/system/${SERVICE_NAME}.service" << EOF
[Unit]
Description=Cloudnan Agent
After=network.target

[Service]
Type=simple
User=root
ExecStart=${INSTALL_DIR}/${SERVICE_NAME} -token "${AGENT_TOKEN}" -id "${AGENT_ID}" -panel "${PANEL_URL}"
Restart=always
RestartSec=5
StandardOutput=append:${LOG_FILE}
StandardError=append:${LOG_FILE}
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

[Install]
WantedBy=multi-user.target
EOF
    
    # Reload systemd
    systemctl daemon-reload
    
    print_success "Systemd service created"
}

# Enable and start service
start_service() {
    print_step "Starting agent service..."
    
    systemctl enable "$SERVICE_NAME" --quiet
    systemctl start "$SERVICE_NAME"
    
    # Wait for registration to complete
    print_step "Waiting for certificate registration..."
    sleep 5
    
    if systemctl is-active --quiet "$SERVICE_NAME"; then
        print_success "Agent service started successfully"
    else
        print_warning "Service may not have started correctly. Check logs with: journalctl -u $SERVICE_NAME"
    fi
}

# Uninstall agent
uninstall_agent() {
    print_step "Uninstalling Cloudnan Agent..."
    
    # Stop and disable service
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        print_step "Stopping service..."
        systemctl stop "$SERVICE_NAME"
    fi
    
    if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
        print_step "Disabling service..."
        systemctl disable "$SERVICE_NAME" --quiet
    fi
    
    # Remove service file
    if [ -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
        print_step "Removing systemd service..."
        rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
        systemctl daemon-reload
    fi
    
    # Remove binary
    if [ -f "$INSTALL_DIR/$SERVICE_NAME" ]; then
        print_step "Removing binary..."
        rm -f "$INSTALL_DIR/$SERVICE_NAME"
    fi
    
    # Remove config and PKI directories
    if [ -d "$CONFIG_DIR" ]; then
        print_step "Removing configuration and certificates..."
        rm -rf "$CONFIG_DIR"
    fi
    
    # Remove log file
    if [ -f "$LOG_FILE" ]; then
        print_step "Removing log file..."
        rm -f "$LOG_FILE"
    fi
    
    print_success "Cloudnan Agent uninstalled successfully"
    echo ""
    echo "All agent files have been removed:"
    echo "  - Service: /etc/systemd/system/${SERVICE_NAME}.service"
    echo "  - Binary:  $INSTALL_DIR/$SERVICE_NAME"
    echo "  - Config:  $CONFIG_DIR/"
    echo "  - Logs:    $LOG_FILE"
}

# Upgrade agent (OTA update)
upgrade_agent() {
    print_step "Upgrading Cloudnan Agent..."
    
    # Check if agent is installed
    if [ ! -f "$INSTALL_DIR/$SERVICE_NAME" ]; then
        print_error "Agent not installed. Run install first."
        exit 1
    fi
    
    # Get current version before upgrade
    CURRENT_VERSION=$("$INSTALL_DIR/$SERVICE_NAME" -version 2>/dev/null || echo "unknown")
    print_step "Current version: $CURRENT_VERSION"
    
    # Detect architecture
    detect_arch
    
    # Download new binary to temp location first
    print_step "Downloading new agent binary..."
    TEMP_BINARY="/tmp/cloudnan-agent-new"
    if command -v curl &> /dev/null; then
        curl -sSL "$BINARY_URL" -o "$TEMP_BINARY"
    elif command -v wget &> /dev/null; then
        wget -q "$BINARY_URL" -O "$TEMP_BINARY"
    else
        print_error "Neither curl nor wget found."
        exit 1
    fi
    chmod +x "$TEMP_BINARY"
    print_success "Downloaded new binary"
    
    # Create upgrade script that will run after we exit
    UPGRADE_SCRIPT="/tmp/cloudnan-upgrade-finish.sh"
    cat > "$UPGRADE_SCRIPT" << 'UPGRADE_EOF'
#!/bin/bash
sleep 2
systemctl stop cloudnan-agent 2>/dev/null || true
sleep 1
cp /tmp/cloudnan-agent-new /usr/local/bin/cloudnan-agent
chmod +x /usr/local/bin/cloudnan-agent
rm -f /tmp/cloudnan-agent-new
systemctl start cloudnan-agent
rm -f /tmp/cloudnan-upgrade-finish.sh
UPGRADE_EOF
    chmod +x "$UPGRADE_SCRIPT"
    
    print_step "Starting upgrade process..."
    
    # Run the upgrade script in background, detached from this process
    nohup "$UPGRADE_SCRIPT" > /var/log/cloudnan-upgrade.log 2>&1 &
    
    print_success "Upgrade initiated! Agent will restart shortly."
    echo ""
    echo "The agent will:"
    echo "  1. Stop the current service"
    echo "  2. Replace the binary"
    echo "  3. Start the new version"
    echo ""
    echo "Check status in a few seconds with: systemctl status cloudnan-agent"
}

# Print completion message
print_completion() {
    echo ""
    echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
    echo -e "${GREEN}  Installation Complete!${NC}"
    echo -e "${GREEN}═══════════════════════════════════════════════════════════${NC}"
    echo ""
    echo "Agent ID:     $AGENT_ID"
    echo "Panel:        $PANEL_URL"
    echo ""
    echo "Useful commands:"
    echo "  View logs:     tail -f $LOG_FILE"
    echo "  Check status:  systemctl status $SERVICE_NAME"
    echo "  Restart:       systemctl restart $SERVICE_NAME"
    echo "  Stop:          systemctl stop $SERVICE_NAME"
    echo "  Uninstall:     curl -sSL https://cloudnan.com/downloads/install.sh | sudo bash -s -- --uninstall"
    echo ""
    echo "Certificates:  $PKI_DIR/"
    echo "Config file:   $CONFIG_DIR/agent.yaml"
    echo "Binary:        $INSTALL_DIR/$SERVICE_NAME"
    echo ""
}

# Main installation flow
main() {
    print_banner
    parse_args "$@"
    check_root
    
    if [ "$UNINSTALL" = true ]; then
        uninstall_agent
        exit 0
    fi
    
    if [ "$UPGRADE" = true ]; then
        upgrade_agent
        exit 0
    fi
    
    validate_args
    detect_arch
    download_agent
    create_config_dir
    create_systemd_service
    start_service
    print_completion
}

# Run main
main "$@"

