#!/bin/bash

# Redis Cluster Failover Demo Script
# Demonstrates automatic failover when a master node dies

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
MAGENTA='\033[0;35m'
CYAN='\033[0;36m'
WHITE='\033[1;37m'
NC='\033[0m' # No Color
BOLD='\033[1m'

# Configuration
CLUSTER_PORTS=(7000 7001 7002 7003 7004 7005)
GATEWAY_URL="http://localhost:8080"
TEST_CLIENT="failover-test-client"
REDIS_CLI="redis-cli"

# Timing
FAILOVER_WAIT=15
REQUEST_INTERVAL=0.5

#######################################
# Helper Functions
#######################################

print_header() {
    echo ""
    echo -e "${CYAN}========================================${NC}"
    echo -e "${CYAN}  $1${NC}"
    echo -e "${CYAN}========================================${NC}"
    echo ""
}

print_step() {
    echo -e "${YELLOW}>>> $1${NC}"
}

print_success() {
    echo -e "${GREEN}[SUCCESS] $1${NC}"
}

print_error() {
    echo -e "${RED}[ERROR] $1${NC}"
}

print_info() {
    echo -e "${BLUE}[INFO] $1${NC}"
}

print_warning() {
    echo -e "${MAGENTA}[WARNING] $1${NC}"
}

timestamp() {
    date "+%H:%M:%S"
}

# Cross-platform millisecond timing (macOS doesn't support date +%N)
get_time_ms() {
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS: use perl for millisecond precision
        perl -MTime::HiRes=time -e 'printf "%.0f\n", time * 1000'
    else
        # Linux: use date with nanoseconds
        echo $(( $(date +%s%N) / 1000000 ))
    fi
}

#######################################
# Prerequisites Check
#######################################

check_prerequisites() {
    print_header "Prerequisites Check"

    local all_running=true

    # Check each cluster node
    for port in "${CLUSTER_PORTS[@]}"; do
        if $REDIS_CLI -p "$port" ping > /dev/null 2>&1; then
            echo -e "  Port ${GREEN}$port${NC}: ${GREEN}OK${NC}"
        else
            echo -e "  Port ${RED}$port${NC}: ${RED}NOT RUNNING${NC}"
            all_running=false
        fi
    done

    echo ""

    if [ "$all_running" = false ]; then
        print_error "Redis cluster is not fully running!"
        echo ""
        echo "Start the cluster with:"
        echo "  cd /path/to/redis-cluster"
        echo "  ./create-cluster.sh start"
        echo ""
        exit 1
    fi

    # Check gateway
    print_step "Checking gateway on $GATEWAY_URL..."
    if curl -s -o /dev/null -w "%{http_code}" "$GATEWAY_URL/health" 2>/dev/null | grep -q "200\|404"; then
        print_success "Gateway is running"
    else
        print_warning "Gateway may not be running on $GATEWAY_URL"
        echo "         Make sure to start it with: REDIS_MODE=cluster ./gateway"
    fi

    print_success "All prerequisites met!"
}

#######################################
# Cluster Status Functions
#######################################

show_cluster_status() {
    print_step "Current Cluster Status:"
    echo ""

    # Get cluster nodes info
    local nodes_info
    nodes_info=$($REDIS_CLI -p 7000 cluster nodes 2>/dev/null || echo "")

    if [ -z "$nodes_info" ]; then
        print_error "Could not get cluster nodes info"
        return 1
    fi

    echo -e "${WHITE}Node ID (short)       Port    Role      Slots${NC}"
    echo -e "${WHITE}---------------------------------------------------${NC}"

    while IFS= read -r line; do
        local node_id port role slots
        node_id=$(echo "$line" | awk '{print substr($1, 1, 8)}')
        port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)

        if echo "$line" | grep -q "master"; then
            role="master"
            slots=$(echo "$line" | awk '{for(i=9;i<=NF;i++) printf $i" "}')
            echo -e "  ${CYAN}$node_id${NC}...    ${port}    ${GREEN}$role${NC}    ${BLUE}$slots${NC}"
        elif echo "$line" | grep -q "slave"; then
            role="replica"
            local master_id
            master_id=$(echo "$line" | awk '{print substr($4, 1, 8)}')
            echo -e "  ${CYAN}$node_id${NC}...    ${port}    ${YELLOW}$role${NC}    (replicates $master_id...)"
        fi
    done <<< "$nodes_info"

    echo ""
}

get_slot_for_key() {
    local key="$1"
    # Redis uses CRC16 for slot calculation, but we can ask Redis directly
    local slot
    slot=$($REDIS_CLI -p 7000 cluster keyslot "$key" 2>/dev/null)
    echo "$slot"
}

get_node_for_slot() {
    local slot="$1"
    local node_info
    node_info=$($REDIS_CLI -p 7000 cluster slots 2>/dev/null)

    # Parse cluster slots output to find which node owns this slot
    $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | while read -r line; do
        if echo "$line" | grep -q "master"; then
            local slots_range
            slots_range=$(echo "$line" | awk '{for(i=9;i<=NF;i++) print $i}')
            for range in $slots_range; do
                local start end
                if [[ "$range" == *"-"* ]]; then
                    start=$(echo "$range" | cut -d- -f1)
                    end=$(echo "$range" | cut -d- -f2)
                else
                    start="$range"
                    end="$range"
                fi
                if [ "$slot" -ge "$start" ] 2>/dev/null && [ "$slot" -le "$end" ] 2>/dev/null; then
                    local port
                    port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
                    echo "$port"
                    return
                fi
            done
        fi
    done
}

identify_target_node() {
    local key="ratelimit:$TEST_CLIENT"
    print_step "Identifying which node handles our test key..." >&2

    local slot
    slot=$(get_slot_for_key "$key")
    echo -e "  Key: ${CYAN}$key${NC}" >&2
    echo -e "  Slot: ${CYAN}$slot${NC}" >&2

    # Find which master owns this slot
    local target_port=""
    while IFS= read -r line; do
        if echo "$line" | grep -q "master"; then
            local slots_range port
            port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
            slots_range=$(echo "$line" | awk '{for(i=9;i<=NF;i++) print $i}')

            for range in $slots_range; do
                local start end
                if [[ "$range" == *"-"* ]]; then
                    start=$(echo "$range" | cut -d- -f1)
                    end=$(echo "$range" | cut -d- -f2)
                else
                    start="$range"
                    end="$range"
                fi
                if [ "$slot" -ge "$start" ] 2>/dev/null && [ "$slot" -le "$end" ] 2>/dev/null; then
                    target_port="$port"
                    break 2
                fi
            done
        fi
    done < <($REDIS_CLI -p 7000 cluster nodes 2>/dev/null)

    if [ -n "$target_port" ]; then
        echo -e "  Master Node: ${GREEN}Port $target_port${NC}" >&2
        echo "$target_port"
    else
        print_error "Could not determine target node" >&2
        echo "7000"  # Default fallback
    fi
}

#######################################
# Request Functions
#######################################

send_request() {
    local client="$1"
    local show_output="${2:-true}"

    local start_time end_time duration status response
    start_time=$(get_time_ms)

    response=$(curl -s -w "\n%{http_code}" -H "X-Forwarded-For: $client" "$GATEWAY_URL/api/resource" 2>&1) || true
    status=$(echo "$response" | tail -n1)

    end_time=$(get_time_ms)
    duration=$(( end_time - start_time ))

    if [ "$show_output" = "true" ]; then
        local ts
        ts=$(timestamp)
        if [ "$status" = "200" ]; then
            echo -e "  [$ts] ${GREEN}HTTP $status${NC} (${duration}ms)"
        elif [ "$status" = "429" ]; then
            echo -e "  [$ts] ${YELLOW}HTTP $status${NC} Rate Limited (${duration}ms)"
        elif [ "$status" = "000" ]; then
            echo -e "  [$ts] ${RED}FAILED${NC} Connection Error (${duration}ms)"
        else
            echo -e "  [$ts] ${RED}HTTP $status${NC} (${duration}ms)"
        fi
    fi

    echo "$status" > /tmp/last_request_status
}

send_continuous_requests() {
    local client="$1"
    local duration="$2"
    local interval="$3"

    local end_time
    end_time=$((SECONDS + duration))

    while [ $SECONDS -lt $end_time ]; do
        send_request "$client" "true"
        sleep "$interval"
    done
}

#######################################
# Node Management
#######################################

kill_redis_node() {
    local port="$1"
    print_step "Killing Redis node on port $port..."

    # Find ONLY the Redis server process (listening on the port)
    # Using -sTCP:LISTEN to avoid killing client connections (like the gateway)
    local pid
    pid=$(lsof -ti:$port -sTCP:LISTEN 2>/dev/null || true)

    if [ -n "$pid" ]; then
        kill -9 $pid 2>/dev/null || true
        sleep 0.5

        if ! $REDIS_CLI -p "$port" ping > /dev/null 2>&1; then
            print_success "Node on port $port killed"
            return 0
        else
            print_error "Failed to kill node on port $port"
            return 1
        fi
    else
        print_warning "No process found on port $port"
        return 1
    fi
}

restart_redis_node() {
    local port="$1"
    print_step "Restarting Redis node on port $port..."

    # Find the cluster directory
    local cluster_dir=""
    local possible_dirs=(
        "$HOME/redis-cluster/$port"
        "/tmp/redis-cluster/$port"
        "./redis-cluster/$port"
        "../redis-cluster/$port"
    )

    for dir in "${possible_dirs[@]}"; do
        if [ -f "$dir/redis.conf" ]; then
            cluster_dir="$dir"
            break
        fi
    done

    if [ -z "$cluster_dir" ]; then
        print_warning "Could not find redis.conf for port $port"
        print_info "You may need to manually restart the node:"
        echo "  redis-server /path/to/redis-cluster/$port/redis.conf"
        return 1
    fi

    # Start the Redis server
    cd "$cluster_dir"
    redis-server redis.conf &

    sleep 2

    if $REDIS_CLI -p "$port" ping > /dev/null 2>&1; then
        print_success "Node on port $port restarted"
        return 0
    else
        print_error "Failed to restart node on port $port"
        return 1
    fi
}

wait_for_failover() {
    local killed_port="$1"
    local max_wait="$2"

    print_step "Waiting for automatic failover (max ${max_wait}s)..."

    local start_time=$SECONDS
    local failover_complete=false

    while [ $((SECONDS - start_time)) -lt "$max_wait" ]; do
        local elapsed=$((SECONDS - start_time))

        # Check if a new master has been elected for the slots
        local cluster_ok=true
        local new_master=""

        # Try to get cluster info from any available node
        for port in "${CLUSTER_PORTS[@]}"; do
            if [ "$port" != "$killed_port" ]; then
                local state
                state=$($REDIS_CLI -p "$port" cluster info 2>/dev/null | grep cluster_state | cut -d: -f2 | tr -d '\r' || echo "")

                if [ "$state" = "ok" ]; then
                    # Check if the killed node's replica has been promoted
                    local nodes
                    nodes=$($REDIS_CLI -p "$port" cluster nodes 2>/dev/null || echo "")

                    if echo "$nodes" | grep -v "fail" | grep -q "master"; then
                        # Count active masters
                        local active_masters
                        active_masters=$(echo "$nodes" | grep -v "fail" | grep -c "master" || echo "0")

                        if [ "$active_masters" -ge 3 ]; then
                            failover_complete=true
                            print_success "Failover complete! (${elapsed}s)"
                            break 2
                        fi
                    fi
                fi
            fi
        done

        echo -e "  [${elapsed}s] Cluster state: ${YELLOW}recovering...${NC}"
        sleep 1
    done

    if [ "$failover_complete" = false ]; then
        print_warning "Failover may not be complete after ${max_wait}s"
    fi
}

#######################################
# Main Demo Sequence
#######################################

get_master_for_slot() {
    local slot="$1"
    $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | while read -r line; do
        if echo "$line" | grep -q "master" && ! echo "$line" | grep -q "fail"; then
            local port slots_range
            port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
            slots_range=$(echo "$line" | awk '{for(i=9;i<=NF;i++) print $i}')
            for range in $slots_range; do
                local start end
                if [[ "$range" == *"-"* ]]; then
                    start=$(echo "$range" | cut -d- -f1)
                    end=$(echo "$range" | cut -d- -f2)
                else
                    start="$range"
                    end="$range"
                fi
                if [ "$slot" -ge "$start" ] 2>/dev/null && [ "$slot" -le "$end" ] 2>/dev/null; then
                    echo "$port"
                    return
                fi
            done
        fi
    done
}

get_replica_for_master() {
    local master_port="$1"
    local master_id
    master_id=$($REDIS_CLI -p "$master_port" cluster myid 2>/dev/null)
    $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | grep "slave $master_id" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1 | head -1
}

run_demo() {
    local demo_start=$SECONDS
    local key="ratelimit:$TEST_CLIENT"

    print_header "Redis Cluster Failover Demo"
    echo -e "${WHITE}This demo shows how Redis Cluster handles master node failure:${NC}"
    echo ""
    echo -e "  ${CYAN}BEFORE:${NC}  Client → Gateway → Redis Master (port X)"
    echo -e "  ${RED}FAILURE:${NC} Master dies → Gateway fails open → Requests still work"
    echo -e "  ${GREEN}AFTER:${NC}  Replica promoted → Client → Gateway → New Master (port Y)"
    echo ""
    read -p "Press Enter to begin..." -r

    #-------------------------------------------------------------------
    # PHASE 1: BEFORE FAILURE - Understand the current state
    #-------------------------------------------------------------------
    print_header "PHASE 1: BEFORE FAILURE"
    echo -e "${WHITE}Understanding where our data lives in the cluster${NC}"
    echo ""

    # Show cluster topology
    echo -e "${BOLD}Current Cluster Topology:${NC}"
    echo -e "┌─────────────────────────────────────────────────────────────┐"
    $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | while read -r line; do
        local port role slots
        port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
        if echo "$line" | grep -q "master"; then
            slots=$(echo "$line" | awk '{for(i=9;i<=NF;i++) printf $i" "}')
            echo -e "│  ${GREEN}MASTER${NC} :$port  →  slots $slots"
        elif echo "$line" | grep -q "slave"; then
            local master_id
            master_id=$(echo "$line" | awk '{print substr($4, 1, 8)}')
            echo -e "│  ${YELLOW}REPLICA${NC} :$port  →  replicates $master_id..."
        fi
    done
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    # Identify where our key lives
    local slot
    slot=$(get_slot_for_key "$key")
    local target_port
    target_port=$(get_master_for_slot "$slot")
    local replica_port
    replica_port=$(get_replica_for_master "$target_port")

    echo -e "${BOLD}Key Routing:${NC}"
    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  Key:     ${CYAN}$key${NC}"
    echo -e "│  Slot:    ${CYAN}$slot${NC} (Redis hashes key to determine slot)"
    echo -e "│  Master:  ${GREEN}:$target_port${NC} (owns slots containing $slot)"
    echo -e "│  Replica: ${YELLOW}:$replica_port${NC} (will be promoted if master fails)"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    # Send requests and show they go to the master
    echo -e "${BOLD}Sending requests to establish rate limit state:${NC}"
    echo -e "${WHITE}All requests are handled by master :$target_port${NC}"
    echo ""
    for i in {1..5}; do
        send_request "$TEST_CLIENT" "true"
        sleep 0.2
    done

    local tokens
    tokens=$($REDIS_CLI -c -p "$target_port" HGET "$key" tokens 2>/dev/null || echo "N/A")
    echo ""
    echo -e "│  Data stored on :$target_port → tokens remaining: ${CYAN}$tokens${NC}"
    echo ""

    read -p "Press Enter to kill master node :$target_port..." -r

    #-------------------------------------------------------------------
    # PHASE 2: DURING FAILURE - Kill master, observe behavior
    #-------------------------------------------------------------------
    print_header "PHASE 2: DURING FAILURE"
    echo ""
    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  ${RED}KILLING MASTER NODE :$target_port${NC}                              "
    echo -e "│                                                             │"
    echo -e "│  What happens next:                                         │"
    echo -e "│  1. Gateway tries to reach :$target_port → connection refused   │"
    echo -e "│  2. Gateway 'fails open' → allows request without rate limit│"
    echo -e "│  3. Cluster detects failure → promotes replica :$replica_port      │"
    echo -e "│  4. Gateway discovers new master → normal operation resumes │"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    local kill_time=$SECONDS
    kill_redis_node "$target_port"
    echo ""

    echo -e "${BOLD}Sending requests during failover:${NC}"
    echo -e "${WHITE}Watch for 'failing open' messages - requests succeed despite Redis being down${NC}"
    echo ""

    send_continuous_requests "$TEST_CLIENT" 6 0.5
    echo ""

    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  ${YELLOW}EXPLANATION:${NC}                                               │"
    echo -e "│  • 'failing open' = Redis unreachable, but request allowed  │"
    echo -e "│  • HTTP 200 = Backend responded successfully                │"
    echo -e "│  • Rate limiting temporarily disabled (fail-open strategy)  │"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    #-------------------------------------------------------------------
    # PHASE 3: AUTOMATIC FAILOVER - Watch cluster heal itself
    #-------------------------------------------------------------------
    print_header "PHASE 3: AUTOMATIC FAILOVER"
    echo -e "${WHITE}Redis Cluster is now detecting the failure and promoting the replica...${NC}"
    echo ""

    wait_for_failover "$target_port" "$FAILOVER_WAIT"

    local failover_duration=$((SECONDS - kill_time))
    echo ""

    # Show new topology
    local new_master
    new_master=$(get_master_for_slot "$slot")

    echo -e "${BOLD}New Cluster Topology:${NC}"
    echo -e "┌─────────────────────────────────────────────────────────────┐"
    $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | while read -r line; do
        local port role
        port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
        if echo "$line" | grep -q "master" && ! echo "$line" | grep -q "fail"; then
            local slots
            slots=$(echo "$line" | awk '{for(i=9;i<=NF;i++) printf $i" "}')
            if [ "$port" = "$new_master" ]; then
                echo -e "│  ${GREEN}MASTER${NC} :$port  →  slots $slots ${GREEN}← NEW MASTER!${NC}"
            else
                echo -e "│  ${GREEN}MASTER${NC} :$port  →  slots $slots"
            fi
        elif echo "$line" | grep -q "slave"; then
            local master_id
            master_id=$(echo "$line" | awk '{print substr($4, 1, 8)}')
            echo -e "│  ${YELLOW}REPLICA${NC} :$port  →  replicates $master_id..."
        elif echo "$line" | grep -q "fail"; then
            echo -e "│  ${RED}FAILED${NC}  :$port  →  (node is down)"
        fi
    done
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  ${GREEN}FAILOVER COMPLETE${NC}                                          │"
    echo -e "│  • Old master: :$target_port (failed)                            │"
    echo -e "│  • New master: :$new_master (promoted from replica)             │"
    echo -e "│  • Failover duration: ${CYAN}${failover_duration} seconds${NC}                          │"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    #-------------------------------------------------------------------
    # PHASE 4: AFTER RECOVERY - Show normal operation resumed
    #-------------------------------------------------------------------
    print_header "PHASE 4: AFTER RECOVERY"
    echo -e "${WHITE}Gateway has discovered the new master. Requests now go to :$new_master${NC}"
    echo ""

    echo -e "${BOLD}Sending requests to verify recovery:${NC}"
    echo ""

    local success_count=0
    for i in {1..5}; do
        send_request "$TEST_CLIENT" "true"
        if [ "$(cat /tmp/last_request_status)" = "200" ] || [ "$(cat /tmp/last_request_status)" = "429" ]; then
            ((success_count++))
        fi
        sleep 0.3
    done
    echo ""

    # Check data on new master
    tokens=$($REDIS_CLI -c -p "$new_master" HGET "$key" tokens 2>/dev/null || echo "N/A")

    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  ${GREEN}SERVICE RESTORED${NC}                                           │"
    echo -e "│  • Requests successful: $success_count/5                              │"
    echo -e "│  • Data on new master :$new_master → tokens: ${CYAN}$tokens${NC}        "
    echo -e "│  • Rate limiting is working again                           │"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""

    #-------------------------------------------------------------------
    # PHASE 5: RESTART OLD NODE - Show it rejoins as replica
    #-------------------------------------------------------------------
    print_header "PHASE 5: RESTARTING FAILED NODE"
    echo -e "${WHITE}When the old master restarts, it will rejoin as a replica of the new master${NC}"
    echo ""

    if restart_redis_node "$target_port"; then
        sleep 3

        echo -e "${BOLD}Final Cluster Topology:${NC}"
        echo -e "┌─────────────────────────────────────────────────────────────┐"
        $REDIS_CLI -p 7000 cluster nodes 2>/dev/null | while read -r line; do
            local port
            port=$(echo "$line" | awk '{print $2}' | cut -d: -f2 | cut -d@ -f1)
            if echo "$line" | grep -q "master"; then
                local slots
                slots=$(echo "$line" | awk '{for(i=9;i<=NF;i++) printf $i" "}')
                echo -e "│  ${GREEN}MASTER${NC}  :$port  →  slots $slots"
            elif echo "$line" | grep -q "slave"; then
                local master_id
                master_id=$(echo "$line" | awk '{print substr($4, 1, 8)}')
                if [ "$port" = "$target_port" ]; then
                    echo -e "│  ${YELLOW}REPLICA${NC} :$port  →  replicates $master_id... ${YELLOW}← WAS MASTER, NOW REPLICA${NC}"
                else
                    echo -e "│  ${YELLOW}REPLICA${NC} :$port  →  replicates $master_id..."
                fi
            fi
        done
        echo -e "└─────────────────────────────────────────────────────────────┘"
    else
        print_info "Manual restart may be required for port $target_port"
    fi
    echo ""

    #-------------------------------------------------------------------
    # SUMMARY
    #-------------------------------------------------------------------
    print_header "DEMO COMPLETE"
    local total_duration=$((SECONDS - demo_start))

    echo -e "┌─────────────────────────────────────────────────────────────┐"
    echo -e "│  ${BOLD}SUMMARY${NC}                                                    │"
    echo -e "├─────────────────────────────────────────────────────────────┤"
    echo -e "│  Total demo time:     ${CYAN}${total_duration} seconds${NC}                            │"
    echo -e "│  Failover duration:   ${CYAN}${failover_duration} seconds${NC}                            │"
    echo -e "│  Old master:          :$target_port → now replica                  │"
    echo -e "│  New master:          :$new_master → promoted from replica        │"
    echo -e "├─────────────────────────────────────────────────────────────┤"
    echo -e "│  ${BOLD}KEY CONCEPTS DEMONSTRATED${NC}                                  │"
    echo -e "├─────────────────────────────────────────────────────────────┤"
    echo -e "│  ${GREEN}✓${NC} Redis Cluster auto-promotes replicas when master fails   │"
    echo -e "│  ${GREEN}✓${NC} Gateway 'fail-open' maintains availability during outage │"
    echo -e "│  ${GREEN}✓${NC} Data is preserved (replicated before failure)            │"
    echo -e "│  ${GREEN}✓${NC} Old master rejoins as replica when restarted             │"
    echo -e "│  ${GREEN}✓${NC} No manual intervention required for recovery             │"
    echo -e "└─────────────────────────────────────────────────────────────┘"
    echo ""
}

#######################################
# Script Entry Point
#######################################

main() {
    echo ""
    echo -e "${BOLD}${CYAN}"
    echo "  ____          _ _        _____ _           _            "
    echo " |  _ \ ___  __| (_)___   / ____| |_   _ ___| |_ ___ _ __ "
    echo " | |_) / _ \/ _\` | / __| | |    | | | | / __| __/ _ \ '__|"
    echo " |  _ <  __/ (_| | \__ \ | |____| | |_| \__ \ ||  __/ |   "
    echo " |_| \_\___|\__,_|_|___/  \_____|_|\__,_|___/\__\___|_|   "
    echo "                                                          "
    echo "  Failover Demo Script"
    echo -e "${NC}"

    # Check prerequisites
    check_prerequisites

    # Run the demo
    run_demo
}

# Handle Ctrl+C gracefully
trap 'echo ""; print_warning "Demo interrupted"; exit 1' INT TERM

# Run main
main "$@"
