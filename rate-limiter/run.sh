#!/bin/bash

set -e

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
BACKEND_PID=""
GATEWAY_PID=""
REDIS_MODE="${REDIS_MODE:-standalone}"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

cleanup() {
    echo -e "\n${YELLOW}Shutting down services...${NC}"
    # Kill by PID if we have them
    [ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" 2>/dev/null || true
    [ -n "$BACKEND_PID" ] && kill "$BACKEND_PID" 2>/dev/null || true
    # Also kill by name to catch any strays
    pkill -f "$ROOT_DIR/backend/backend" 2>/dev/null || true
    pkill -f "$ROOT_DIR/gateway/gateway" 2>/dev/null || true
    sleep 0.5
    echo -e "${GREEN}Done.${NC}"
}

trap cleanup EXIT INT TERM

start_services() {
    # Kill any existing instances first (by name and by port)
    pkill -f "$ROOT_DIR/backend/backend" 2>/dev/null || true
    pkill -f "$ROOT_DIR/gateway/gateway" 2>/dev/null || true
    lsof -ti:8080 -ti:8081 | xargs kill -9 2>/dev/null || true
    sleep 0.5

    # Check Redis based on mode
    echo -e "${YELLOW}Checking Redis ($REDIS_MODE mode)...${NC}"
    if [ "$REDIS_MODE" = "cluster" ]; then
        if ! redis-cli -p 7000 ping > /dev/null 2>&1; then
            echo -e "${RED}Redis cluster is not running.${NC}"
            echo "Start it with: ./scripts/cluster-setup.sh start"
            exit 1
        fi
        echo -e "${GREEN}Redis cluster is running.${NC}"
    else
        if ! redis-cli ping > /dev/null 2>&1; then
            echo -e "${RED}Redis is not running. Start it with: brew services start redis${NC}"
            exit 1
        fi
        echo -e "${GREEN}Redis is running.${NC}"
    fi

    # Build services
    echo -e "${YELLOW}Building backend...${NC}"
    cd "$ROOT_DIR/backend" && go build -o backend .
    echo -e "${GREEN}Backend built.${NC}"

    echo -e "${YELLOW}Building gateway...${NC}"
    cd "$ROOT_DIR/gateway" && go build -o gateway .
    echo -e "${GREEN}Gateway built.${NC}"

    # Start backend
    echo -e "${YELLOW}Starting backend on :8081...${NC}"
    cd "$ROOT_DIR/backend" && ./backend &
    BACKEND_PID=$!
    sleep 1

    if ! kill -0 "$BACKEND_PID" 2>/dev/null; then
        echo -e "${RED}Backend failed to start${NC}"
        exit 1
    fi
    echo -e "${GREEN}Backend running (PID: $BACKEND_PID)${NC}"

    # Start gateway with appropriate Redis config
    echo -e "${YELLOW}Starting gateway on :8080 ($REDIS_MODE mode)...${NC}"
    cd "$ROOT_DIR/gateway"

    if [ "$REDIS_MODE" = "cluster" ]; then
        REDIS_MODE=cluster \
        REDIS_ADDRS="localhost:7000,localhost:7001,localhost:7002" \
        BACKEND_URL=http://localhost:8081 \
        BUCKET_SIZE=10 \
        REFILL_RATE=1 \
        ./gateway &
    else
        REDIS_MODE=standalone \
        REDIS_ADDR=localhost:6379 \
        BACKEND_URL=http://localhost:8081 \
        BUCKET_SIZE=10 \
        REFILL_RATE=1 \
        ./gateway &
    fi

    GATEWAY_PID=$!
    sleep 1

    if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
        echo -e "${RED}Gateway failed to start${NC}"
        exit 1
    fi
    echo -e "${GREEN}Gateway running (PID: $GATEWAY_PID)${NC}"

    # Wait for services to be ready
    echo -e "${YELLOW}Waiting for services to be ready...${NC}"
    for i in {1..10}; do
        if curl -s http://localhost:8080/health > /dev/null 2>&1; then
            echo -e "${GREEN}Services are ready!${NC}"
            break
        fi
        sleep 1
    done

    echo ""
    echo -e "${GREEN}========================================${NC}"
    echo -e "${GREEN}  Rate Limiter System Running${NC}"
    echo -e "${GREEN}========================================${NC}"
    echo -e "  Gateway: http://localhost:8080"
    echo -e "  Backend: http://localhost:8081"
    if [ "$REDIS_MODE" = "cluster" ]; then
        echo -e "  Redis:   cluster (ports 7000-7005)"
    else
        echo -e "  Redis:   localhost:6379"
    fi
    echo -e "${GREEN}========================================${NC}"
    echo ""
}

run_demo() {
    echo -e "${YELLOW}Sending 12 requests (bucket size is 10)...${NC}"
    echo ""
    for i in {1..12}; do
        echo -n "Request $i: "
        STATUS=$(curl -s -o /dev/null -w "%{http_code}" -H "X-Forwarded-For: demo-client" http://localhost:8080/api/resource)
        if [ "$STATUS" = "200" ]; then
            echo -e "${GREEN}$STATUS OK${NC}"
        else
            echo -e "${RED}$STATUS Too Many Requests${NC}"
        fi
    done
}

usage() {
    echo -e "${CYAN}Rate Limiter - Run Script${NC}"
    echo ""
    echo "Usage: $0 [command]"
    echo "       REDIS_MODE=cluster $0 [command]"
    echo ""
    echo "Commands:"
    echo "  demo      - Start services and send 12 requests to see rate limiting"
    echo "  test      - Start services and run integration tests"
    echo "  (none)    - Start services and keep running (Ctrl+C to stop)"
    echo ""
    echo "Cluster Commands:"
    echo "  cluster-start    - Start Redis cluster (6 nodes)"
    echo "  cluster-stop     - Stop Redis cluster"
    echo "  cluster-status   - Show Redis cluster status"
    echo "  cluster-demo     - Run failover demo (cluster must be running)"
    echo ""
    echo "Examples:"
    echo "  $0 demo                      # Standalone Redis demo"
    echo "  REDIS_MODE=cluster $0 demo   # Cluster mode demo"
    echo "  $0 cluster-start             # Start Redis cluster"
    echo "  $0 cluster-demo              # Run failover demo"
    echo ""
}

# Handle command
case "${1:-}" in
    test)
        start_services
        echo -e "${YELLOW}Running integration tests...${NC}"
        cd "$ROOT_DIR/tests" && go test -v -count=1 ./...
        ;;
    demo)
        start_services
        run_demo
        ;;
    cluster-start)
        exec "$ROOT_DIR/scripts/cluster-setup.sh" start
        ;;
    cluster-stop)
        exec "$ROOT_DIR/scripts/cluster-setup.sh" stop
        ;;
    cluster-status)
        exec "$ROOT_DIR/scripts/cluster-setup.sh" status
        ;;
    cluster-demo)
        # For cluster demo, set cluster mode and run failover demo
        if ! redis-cli -p 7000 ping > /dev/null 2>&1; then
            echo -e "${RED}Redis cluster is not running.${NC}"
            echo "Start it with: $0 cluster-start"
            exit 1
        fi
        REDIS_MODE=cluster
        start_services
        exec "$ROOT_DIR/scripts/failover-demo.sh"
        ;;
    -h|--help|help)
        usage
        ;;
    "")
        start_services
        echo "Press Ctrl+C to stop services..."
        wait
        ;;
    *)
        echo -e "${RED}Unknown command: $1${NC}"
        echo ""
        usage
        exit 1
        ;;
esac
