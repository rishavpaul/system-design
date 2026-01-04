#!/bin/bash

#######################################
# Redis Cluster Setup Script
# Creates a 6-node Redis cluster locally
# 3 masters + 3 replicas for failover testing
# Ports: 7000-7005
#######################################

set -e

# Configuration
CLUSTER_DIR="/tmp/redis-cluster"
PORTS=(7000 7001 7002 7003 7004 7005)
REDIS_HOST="127.0.0.1"
REPLICAS=1

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color
BOLD='\033[1m'

#######################################
# Print functions with colors
#######################################
print_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_header() {
    echo -e "\n${BOLD}${CYAN}========================================${NC}"
    echo -e "${BOLD}${CYAN}  $1${NC}"
    echo -e "${BOLD}${CYAN}========================================${NC}\n"
}

#######################################
# Check if Redis is installed
#######################################
check_redis_installed() {
    if ! command -v redis-server &> /dev/null; then
        print_error "redis-server is not installed or not in PATH"
        print_info "Install Redis using:"
        echo "  - macOS: brew install redis"
        echo "  - Ubuntu: sudo apt-get install redis-server"
        exit 1
    fi

    if ! command -v redis-cli &> /dev/null; then
        print_error "redis-cli is not installed or not in PATH"
        exit 1
    fi

    print_success "Redis is installed: $(redis-server --version | head -1)"
}

#######################################
# Check if a port is in use
#######################################
is_port_in_use() {
    local port=$1
    if lsof -i :"$port" &> /dev/null; then
        return 0  # Port is in use
    else
        return 1  # Port is free
    fi
}

#######################################
# Generate redis.conf for a node
#######################################
generate_config() {
    local port=$1
    local node_dir="${CLUSTER_DIR}/${port}"
    local config_file="${node_dir}/redis.conf"

    mkdir -p "$node_dir"

    cat > "$config_file" << EOF
# Redis Cluster Node Configuration
# Port: ${port}

port ${port}
bind 127.0.0.1
daemonize yes
pidfile ${node_dir}/redis.pid
logfile ${node_dir}/redis.log
dir ${node_dir}

# Cluster configuration
cluster-enabled yes
cluster-config-file ${node_dir}/nodes.conf
cluster-node-timeout 5000

# Persistence (optional, can be disabled for testing)
appendonly yes
appendfilename "appendonly.aof"

# Memory management
maxmemory 100mb
maxmemory-policy allkeys-lru

# Additional settings for local development
protected-mode no
tcp-backlog 511
timeout 0
tcp-keepalive 300
loglevel notice
databases 16
EOF

    print_info "Generated config for node on port ${port}"
}

#######################################
# Start a single Redis node
#######################################
start_node() {
    local port=$1
    local node_dir="${CLUSTER_DIR}/${port}"
    local config_file="${node_dir}/redis.conf"

    if is_port_in_use "$port"; then
        print_warning "Port ${port} is already in use, skipping..."
        return 1
    fi

    if [[ ! -f "$config_file" ]]; then
        print_error "Config file not found for port ${port}"
        return 1
    fi

    redis-server "$config_file"

    # Wait for the node to start
    local max_attempts=10
    local attempt=0
    while ! redis-cli -p "$port" ping &> /dev/null; do
        sleep 0.5
        ((attempt++))
        if [[ $attempt -ge $max_attempts ]]; then
            print_error "Failed to start Redis node on port ${port}"
            return 1
        fi
    done

    print_success "Started Redis node on port ${port}"
    return 0
}

#######################################
# Stop a single Redis node
#######################################
stop_node() {
    local port=$1

    if redis-cli -p "$port" ping &> /dev/null 2>&1; then
        redis-cli -p "$port" shutdown nosave &> /dev/null 2>&1 || true
        print_info "Stopped Redis node on port ${port}"
    else
        print_warning "Redis node on port ${port} is not running"
    fi
}

#######################################
# Start all nodes and create cluster
#######################################
start_cluster() {
    print_header "Starting Redis Cluster"

    # Check Redis installation
    check_redis_installed

    # Check if any nodes are already running
    local running_nodes=0
    for port in "${PORTS[@]}"; do
        if is_port_in_use "$port"; then
            ((running_nodes++))
        fi
    done

    if [[ $running_nodes -gt 0 ]]; then
        print_warning "Some Redis nodes are already running"
        read -p "Do you want to stop them and start fresh? (y/n): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            stop_cluster
        else
            print_error "Cannot start cluster with existing nodes. Run 'stop' first."
            exit 1
        fi
    fi

    # Create cluster directory
    print_info "Creating cluster directory: ${CLUSTER_DIR}"
    mkdir -p "$CLUSTER_DIR"

    # Generate configs and start nodes
    print_info "Generating configurations and starting nodes..."
    local started_nodes=0
    for port in "${PORTS[@]}"; do
        generate_config "$port"
        if start_node "$port"; then
            ((started_nodes++))
        fi
    done

    if [[ $started_nodes -ne ${#PORTS[@]} ]]; then
        print_error "Failed to start all nodes. Started: ${started_nodes}/${#PORTS[@]}"
        print_info "Cleaning up..."
        stop_cluster
        exit 1
    fi

    print_success "All ${#PORTS[@]} nodes started successfully"

    # Create the cluster
    print_info "Creating Redis cluster..."

    # Build the node addresses string
    local nodes=""
    for port in "${PORTS[@]}"; do
        nodes="${nodes} ${REDIS_HOST}:${port}"
    done

    # Create cluster with replicas
    echo "yes" | redis-cli --cluster create $nodes --cluster-replicas $REPLICAS

    if [[ $? -eq 0 ]]; then
        print_success "Redis cluster created successfully!"
        echo
        show_status
    else
        print_error "Failed to create Redis cluster"
        exit 1
    fi
}

#######################################
# Stop all nodes
#######################################
stop_cluster() {
    print_header "Stopping Redis Cluster"

    for port in "${PORTS[@]}"; do
        stop_node "$port"
    done

    # Also kill any stray redis processes on cluster ports
    for port in "${PORTS[@]}"; do
        local pid=$(lsof -t -i :"$port" 2>/dev/null || true)
        if [[ -n "$pid" ]]; then
            kill -9 "$pid" 2>/dev/null || true
            print_info "Killed process on port ${port}"
        fi
    done

    print_success "All Redis cluster nodes stopped"
}

#######################################
# Show cluster status
#######################################
show_status() {
    print_header "Redis Cluster Status"

    local running=0
    local stopped=0

    echo -e "${BOLD}Node Status:${NC}"
    echo "----------------------------------------"

    for port in "${PORTS[@]}"; do
        if redis-cli -p "$port" ping &> /dev/null 2>&1; then
            echo -e "  Port ${port}: ${GREEN}RUNNING${NC}"
            ((running++))
        else
            echo -e "  Port ${port}: ${RED}STOPPED${NC}"
            ((stopped++))
        fi
    done

    echo "----------------------------------------"
    echo -e "Running: ${GREEN}${running}${NC} | Stopped: ${RED}${stopped}${NC}"
    echo

    # Show cluster info if at least one node is running
    if [[ $running -gt 0 ]]; then
        local first_running_port=""
        for port in "${PORTS[@]}"; do
            if redis-cli -p "$port" ping &> /dev/null 2>&1; then
                first_running_port=$port
                break
            fi
        done

        if [[ -n "$first_running_port" ]]; then
            echo -e "${BOLD}Cluster Info:${NC}"
            echo "----------------------------------------"
            redis-cli -p "$first_running_port" cluster info 2>/dev/null | grep -E "^cluster_state|^cluster_slots_assigned|^cluster_slots_ok|^cluster_known_nodes|^cluster_size" | while read line; do
                echo "  $line"
            done
            echo "----------------------------------------"

            echo
            echo -e "${BOLD}Cluster Nodes:${NC}"
            echo "----------------------------------------"
            redis-cli -p "$first_running_port" cluster nodes 2>/dev/null | while read line; do
                # Colorize master/slave
                if echo "$line" | grep -q "master"; then
                    echo -e "  ${CYAN}$line${NC}"
                elif echo "$line" | grep -q "slave"; then
                    echo -e "  ${YELLOW}$line${NC}"
                else
                    echo "  $line"
                fi
            done
            echo "----------------------------------------"
        fi
    fi
}

#######################################
# Clean up all temp files
#######################################
clean_cluster() {
    print_header "Cleaning Redis Cluster"

    # Stop all nodes first
    stop_cluster

    # Remove cluster directory
    if [[ -d "$CLUSTER_DIR" ]]; then
        print_info "Removing cluster directory: ${CLUSTER_DIR}"
        rm -rf "$CLUSTER_DIR"
        print_success "Cluster directory removed"
    else
        print_info "Cluster directory does not exist"
    fi

    print_success "Cleanup complete"
}

#######################################
# Print usage
#######################################
usage() {
    echo -e "${BOLD}Redis Cluster Setup Script${NC}"
    echo
    echo "Usage: $0 {start|stop|status|clean}"
    echo
    echo "Commands:"
    echo "  start   - Create and start a 6-node Redis cluster"
    echo "  stop    - Stop all Redis cluster nodes"
    echo "  status  - Show cluster status"
    echo "  clean   - Stop all nodes and remove temp files"
    echo
    echo "Configuration:"
    echo "  Cluster directory: ${CLUSTER_DIR}"
    echo "  Ports: ${PORTS[*]}"
    echo "  Nodes: 3 masters + 3 replicas"
    echo
}

#######################################
# Main
#######################################
main() {
    case "${1:-}" in
        start)
            start_cluster
            ;;
        stop)
            stop_cluster
            ;;
        status)
            show_status
            ;;
        clean)
            clean_cluster
            ;;
        *)
            usage
            exit 1
            ;;
    esac
}

main "$@"
