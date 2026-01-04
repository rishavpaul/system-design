package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gatewayURL   = "http://localhost:8080"
	redisAddr    = "localhost:6379"
	bucketSize   = 10
	refillRate   = 1.0 // tokens per second
)

// Helper to clear rate limit state for a client
func clearRateLimitState(t *testing.T, clientIP string) {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer client.Close()

	key := "ratelimit:" + clientIP
	err := client.Del(ctx, key).Err()
	require.NoError(t, err, "Failed to clear rate limit state")
}

// Helper to make a request with a specific client IP
func makeRequest(t *testing.T, clientIP string) (*http.Response, error) {
	req, err := http.NewRequest("GET", gatewayURL+"/api/resource", nil)
	require.NoError(t, err)

	req.Header.Set("X-Forwarded-For", clientIP)

	client := &http.Client{Timeout: 5 * time.Second}
	return client.Do(req)
}

// TestRequestsWithinLimit verifies that requests under the limit succeed
func TestRequestsWithinLimit(t *testing.T) {
	clientIP := "10.0.0.1"
	clearRateLimitState(t, clientIP)

	// Make requests up to the bucket size
	for i := 0; i < bucketSize; i++ {
		resp, err := makeRequest(t, clientIP)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode,
			"Request %d should succeed", i+1)
	}
}

// TestRequestsExceedLimit verifies that requests over the limit get 429
func TestRequestsExceedLimit(t *testing.T) {
	clientIP := "10.0.0.2"
	clearRateLimitState(t, clientIP)

	// Exhaust the bucket
	for i := 0; i < bucketSize; i++ {
		resp, err := makeRequest(t, clientIP)
		require.NoError(t, err)
		resp.Body.Close()
	}

	// Next request should be rate limited
	resp, err := makeRequest(t, clientIP)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"Request exceeding limit should get 429")

	// Verify error response body
	body, _ := io.ReadAll(resp.Body)
	var errResp map[string]any
	err = json.Unmarshal(body, &errResp)
	require.NoError(t, err)
	assert.Equal(t, "rate limit exceeded", errResp["error"])
}

// TestTokenRefill verifies that tokens are refilled over time
func TestTokenRefill(t *testing.T) {
	clientIP := "10.0.0.3"
	clearRateLimitState(t, clientIP)

	// Exhaust the bucket
	for i := 0; i < bucketSize; i++ {
		resp, err := makeRequest(t, clientIP)
		require.NoError(t, err)
		resp.Body.Close()
	}

	// Verify we're rate limited
	resp, err := makeRequest(t, clientIP)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	resp.Body.Close()

	// Wait for tokens to refill (at least 1 token)
	time.Sleep(1500 * time.Millisecond)

	// Should be able to make another request
	resp, err = makeRequest(t, clientIP)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Request should succeed after token refill")
}

// TestBurstAllowed verifies that bursts up to bucket size are allowed
func TestBurstAllowed(t *testing.T) {
	clientIP := "10.0.0.4"
	clearRateLimitState(t, clientIP)

	// Make all requests as fast as possible (burst)
	var wg sync.WaitGroup
	results := make([]int, bucketSize)

	for i := 0; i < bucketSize; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := makeRequest(t, clientIP)
			if err != nil {
				results[idx] = -1
				return
			}
			results[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}

	wg.Wait()

	// All burst requests should succeed
	successCount := 0
	for _, status := range results {
		if status == http.StatusOK {
			successCount++
		}
	}

	assert.Equal(t, bucketSize, successCount,
		"All burst requests up to bucket size should succeed")
}

// TestRateLimitHeaders verifies correct headers are returned
func TestRateLimitHeaders(t *testing.T) {
	clientIP := "10.0.0.5"
	clearRateLimitState(t, clientIP)

	// First request
	resp, err := makeRequest(t, clientIP)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Check headers
	limit := resp.Header.Get("X-RateLimit-Limit")
	remaining := resp.Header.Get("X-RateLimit-Remaining")

	assert.Equal(t, strconv.Itoa(bucketSize), limit,
		"X-RateLimit-Limit should equal bucket size")

	remainingInt, err := strconv.Atoi(remaining)
	require.NoError(t, err)
	assert.Equal(t, bucketSize-1, remainingInt,
		"X-RateLimit-Remaining should be bucket size minus 1")

	// Exhaust and check retry-after header
	for i := 0; i < bucketSize; i++ {
		resp, _ := makeRequest(t, clientIP)
		if resp != nil {
			resp.Body.Close()
		}
	}

	resp, err = makeRequest(t, clientIP)
	require.NoError(t, err)
	defer resp.Body.Close()

	retryAfter := resp.Header.Get("X-RateLimit-Retry-After")
	assert.NotEmpty(t, retryAfter, "X-RateLimit-Retry-After should be set when rate limited")
}


// TestConcurrentRequests verifies correct behavior under concurrent load
func TestConcurrentRequests(t *testing.T) {
	clientIP := "10.0.0.8"
	clearRateLimitState(t, clientIP)

	// Make more concurrent requests than bucket size
	numRequests := bucketSize + 5
	var successCount atomic.Int32
	var rateLimitedCount atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := makeRequest(t, clientIP)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			switch resp.StatusCode {
			case http.StatusOK:
				successCount.Add(1)
			case http.StatusTooManyRequests:
				rateLimitedCount.Add(1)
			}
		}()
	}

	wg.Wait()

	// Due to race conditions in concurrent requests, we should have roughly
	// bucketSize successes, but might vary slightly due to timing
	assert.LessOrEqual(t, int(successCount.Load()), bucketSize+1,
		"Successful requests should not exceed bucket size (with small margin)")
	assert.GreaterOrEqual(t, int(rateLimitedCount.Load()), 4,
		"Some requests should be rate limited")
}

// TestDifferentClients verifies that different clients have separate limits
func TestDifferentClients(t *testing.T) {
	client1IP := "10.0.0.100"
	client2IP := "10.0.0.101"

	clearRateLimitState(t, client1IP)
	clearRateLimitState(t, client2IP)

	// Exhaust client 1's bucket
	for i := 0; i < bucketSize; i++ {
		resp, err := makeRequest(t, client1IP)
		require.NoError(t, err)
		resp.Body.Close()
	}

	// Client 1 should be rate limited
	resp1, err := makeRequest(t, client1IP)
	require.NoError(t, err)
	defer resp1.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp1.StatusCode,
		"Client 1 should be rate limited")

	// Client 2 should still be able to make requests
	resp2, err := makeRequest(t, client2IP)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode,
		"Client 2 should not be affected by Client 1's rate limit")
}

// TestBackendResponsePassthrough verifies backend response is correctly proxied
func TestBackendResponsePassthrough(t *testing.T) {
	clientIP := "10.0.0.9"
	clearRateLimitState(t, clientIP)

	resp, err := makeRequest(t, clientIP)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var data map[string]any
	err = json.Unmarshal(body, &data)
	require.NoError(t, err)

	// Verify backend response structure
	assert.Contains(t, data, "id")
	assert.Contains(t, data, "name")
	assert.Contains(t, data, "timestamp")
}

// TestPostRequest verifies POST requests work through the gateway
func TestPostRequest(t *testing.T) {
	clientIP := "10.0.0.10"
	clearRateLimitState(t, clientIP)

	reqBody := `{"message": "hello"}`
	req, err := http.NewRequest("POST", gatewayURL+"/api/resource",
		strings.NewReader(reqBody))
	require.NoError(t, err)

	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var data map[string]any
	err = json.Unmarshal(body, &data)
	require.NoError(t, err)

	assert.Equal(t, true, data["echo"])
}

// TestHealthEndpoint verifies the health endpoint works
func TestHealthEndpoint(t *testing.T) {
	clientIP := "10.0.0.11"

	req, err := http.NewRequest("GET", gatewayURL+"/health", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", clientIP)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestMain runs setup before tests
func TestMain(m *testing.M) {
	// Wait for services to be ready
	fmt.Println("Waiting for services to be ready...")

	// Check gateway
	for i := 0; i < 30; i++ {
		resp, err := http.Get(gatewayURL + "/health")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(time.Second)
	}

	// Check Redis
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	for i := 0; i < 30; i++ {
		if err := client.Ping(ctx).Err(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	client.Close()

	fmt.Println("Services ready, running tests...")
	m.Run()
}
