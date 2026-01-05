package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rate-limiter/gateway/ratelimiter"
	"github.com/redis/go-redis/v9"
)

type Gateway struct {
	limiter    *ratelimiter.TokenBucket
	proxy      *httputil.ReverseProxy
	redisAlive bool
}

func main() {
	// Load configuration from environment
	bucketSize := getEnvInt("BUCKET_SIZE", 10)
	refillRate := getEnvFloat("REFILL_RATE", 1.0)
	redisMode := getEnv("REDIS_MODE", "standalone")
	backendURL := getEnv("BACKEND_URL", "http://localhost:8081")

	// Initialize Redis client based on mode
	var redisClient redis.Cmdable
	if redisMode == "cluster" {
		// CLUSTER MODE ROUTING EXPLANATION:
		// Redis Cluster automatically shards data across multiple nodes using consistent hashing.
		// Key routing process:
		//   1. Client computes CRC16(key) % 16384 to get the hash slot (0-16383)
		//   2. Each master node owns a range of hash slots (e.g., master1: 0-5460, master2: 5461-10922, master3: 10923-16383)
		//   3. The redis-cli library automatically routes each command to the correct master based on the key's hash slot
		//   4. If a node returns MOVED/ASK redirect, the client automatically follows the redirect
		//
		// Example: key "ratelimit:192.168.1.1" → CRC16 hash → slot 12345 → routed to master3
		//
		// REPLICA CONFIGURATION:
		// The cluster is configured with REPLICAS=1 (set in cluster-setup.sh line 16)
		// This creates one replica for each master node:
		//   - Master1 (7000) → Replica1 (7003)
		//   - Master2 (7001) → Replica2 (7004)
		//   - Master3 (7002) → Replica3 (7005)
		// Replicas provide:
		//   - High availability: automatic failover if a master dies
		//   - Read scaling: ReadOnly=true allows reads from replicas (see below)
		//
		// SYSTEM DESIGN BENEFIT: This architecture provides horizontal scalability and fault tolerance.
		// Multiple gateway instances can share the same Redis cluster without coordination.

		// Cluster mode: use REDIS_ADDRS (comma-separated list of addresses)
		redisAddrs := getEnv("REDIS_ADDRS", "localhost:7000,localhost:7001,localhost:7002")
		addrs := strings.Split(redisAddrs, ",")
		// Trim whitespace from addresses
		for i := range addrs {
			addrs[i] = strings.TrimSpace(addrs[i])
		}
		redisClient = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:          addrs,
			DialTimeout:    2 * time.Second,
			ReadTimeout:    1 * time.Second,
			WriteTimeout:   1 * time.Second,
			ReadOnly:       true,                    // Allow reads from replicas (read scaling)
			RouteRandomly:  true,                    // Distribute reads across master + replicas (load balancing)
			MaxRetries:     3,                       // Retry on failure (resilience)
		})
		log.Printf("Using Redis Cluster mode with addresses: %v", addrs)
	} else {
		// Standalone mode (default): use REDIS_ADDR
		redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
		redisClient = redis.NewClient(&redis.Options{
			Addr:         redisAddr,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  1 * time.Second,
			WriteTimeout: 1 * time.Second,
		})
		log.Printf("Using Redis standalone mode with address: %s", redisAddr)
	}

	// Test Redis connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("Warning: Redis not available at startup: %v", err)
	}

	// Initialize rate limiter
	limiter := ratelimiter.NewTokenBucket(redisClient, int64(bucketSize), refillRate)

	// Initialize reverse proxy
	target, err := url.Parse(backendURL)
	if err != nil {
		log.Fatal("Invalid backend URL:", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	gateway := &Gateway{
		limiter:    limiter,
		proxy:      proxy,
		redisAlive: true,
	}

	// Start health check goroutine
	go gateway.healthCheckLoop(context.Background())

	// Setup routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", gateway.handleRequest)

	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("Gateway starting on :8080 (bucket_size=%d, refill_rate=%.2f)", bucketSize, refillRate)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Extract client identifier (use IP address)
	clientKey := "ratelimit:" + getClientIP(r)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	// Check rate limit
	result, err := g.limiter.Allow(ctx, clientKey)
	if err != nil {
		// Redis error - fail open (allow request) but log warning
		log.Printf("Rate limiter error (failing open): %v", err)
		w.Header().Set("X-RateLimit-Warning", "rate-limiter-unavailable")
		g.proxy.ServeHTTP(w, r)
		return
	}

	// Set rate limit headers
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(result.Limit, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(result.Remaining, 10))

	if !result.Allowed {
		w.Header().Set("X-RateLimit-Retry-After", strconv.FormatInt(int64(result.RetryAfter.Seconds()), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limit exceeded","retry_after":`+strconv.FormatInt(int64(result.RetryAfter.Seconds()), 10)+`}`)
		return
	}

	// Forward to backend
	g.proxy.ServeHTTP(w, r)
}

func (g *Gateway) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			healthy := g.limiter.IsHealthy(ctx)
			if healthy != g.redisAlive {
				if healthy {
					log.Println("Redis connection restored")
				} else {
					log.Println("Redis connection lost - failing open")
				}
				g.redisAlive = healthy
			}
		}
	}
}

func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	// Fall back to RemoteAddr
	return r.RemoteAddr
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return defaultValue
}
