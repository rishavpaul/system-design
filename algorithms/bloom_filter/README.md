# Bloom Filter - Complete Guide

A space-efficient probabilistic data structure for set membership testing with **zero false negatives** and **configurable false positive rates**.

---

## 1. What is a Bloom Filter?

A Bloom filter answers a single question efficiently: **"Is element X in this set?"**

It provides:
- ✅ **Definitive NO:** If element is not in filter → definitely not in set
- ⚠️ **Probabilistic YES:** If element is in filter → probably in set (with configurable false positive rate)

**Space efficiency:** ~9.6 bits/element for 1% false positive rate vs 64+ bits for storing actual values (85%+ memory savings).

### Core Guarantees

- **Zero false negatives:** If query returns FALSE, element is 100% not in the set
- **Controlled false positives:** If query returns TRUE, element might be in the set (e.g., 1% chance of false positive)
- **One-way structure:** Can only add and check membership, cannot delete or enumerate

### Quick Example

```python
from bloom_filter import BloomFilter

# Create filter: expect 1M items, tolerate 1% false positives
bf = BloomFilter.create_optimal(expected_elements=1_000_000, fp_rate=0.01)

# Add elements
bf.add("user:12345")
bf.add("session:abc-def")

# Query membership
if key not in bf:
    return None  # 100% certain: key doesn't exist, skip expensive lookup

# Might exist - do the expensive operation
result = expensive_database_query(key)
```

---

## 2. How and Why Bloom Filters Work

### The Core Mechanism

A Bloom filter consists of:
1. **Bit array** of size `m` (all initially 0)
2. **k hash functions** (h₁, h₂, ..., hₖ)

#### Adding an Element

```python
# To add "hello":
positions = [h1("hello") % m, h2("hello") % m, ..., hk("hello") % m]
for pos in positions:
    bit_array[pos] = 1  # Set k bits to 1
```

#### Checking Membership

```python
# To check if "hello" exists:
positions = [h1("hello") % m, h2("hello") % m, ..., hk("hello") % m]
all_bits_set = all(bit_array[pos] == 1 for pos in positions)

if not all_bits_set:
    return False  # Definitely NOT in set ✓
else:
    return True   # Probably in set (or false positive)
```

#### Visual Example

```
Adding "hello" (k=3):
h1("hello") = 3  → set bit[3] = 1
h2("hello") = 7  → set bit[7] = 1
h3("hello") = 11 → set bit[11] = 1

Result:
[0][0][0][1][0][0][0][1][0][0][0][1][0][0][0][0]
          ↑           ↑           ↑

Checking "bar" (never added):
h1("bar") = 3  → bit[3] = 1 ✓
h2("bar") = 5  → bit[5] = 1 ✓
h3("bar") = 11 → bit[11] = 1 ✓

All bits set → "Probably in set" (FALSE POSITIVE!)
Bits were actually set by "hello" and other elements
```

### Why False Positives Occur

As more elements are added, more bits get set to 1. Eventually, a random element's k positions might all be set to 1 (by different elements), creating a false positive.

### The Mathematics

**False Positive Probability** after adding `n` elements:

```
P(FP) = (1 - e^(-kn/m))^k

where:
  n = number of elements added
  m = total bits in array
  k = number of hash functions
```

**Optimal Parameters:**
- Optimal size: `m = -(n × ln(p)) / (ln(2))²`
- Optimal hash count: `k = (m/n) × ln(2) ≈ 0.693 × (m/n)`

**Practical values for different false positive rates:**

| FP Rate | Bits/Element | Hash Functions |
|---------|--------------|----------------|
| 10%     | 4.79         | 3              |
| 1%      | 9.59         | 7              |
| 0.1%    | 14.38        | 10             |
| 0.01%   | 19.17        | 13             |

### Why It Works: The Key Insight

You're not storing data—you're storing a **probabilistic summary** of what data exists. This summary is so small it can be:
- Replicated everywhere (only kilobytes)
- Checked in microseconds (O(k) hash operations)
- Used to avoid expensive operations (disk I/O, network calls)

**Result:** 500x-1000x performance improvements by eliminating 99% of unnecessary expensive operations.

---

## 3. Real-World Use Cases

### A. Database Query Optimization (Cassandra, HBase)

**Problem:** Check if a row key exists across multiple SSTables. Each SSTable lookup requires expensive disk I/O.

**Solution:** Each SSTable maintains a Bloom filter of its keys.

```python
class SSTable:
    def get(self, key):
        # Fast in-memory check first
        if key not in self.bloom_filter:
            return None  # Definitely not here - skip disk I/O

        # Might be here - do disk read
        return self._read_from_disk(key)

# Without Bloom filter: 100 disk seeks per query (1-10 seconds)
# With Bloom filter (1% FP): ~1 disk seek per query (10ms)
# Improvement: 500x-1000x faster
```

**Impact:** 99% reduction in unnecessary disk reads.

---

### B. Web Safety (Google Chrome Safe Browsing)

**Problem:** Check if a URL is malicious. Database has 500K+ malicious URLs.

**Constraints:**
- Can't send every URL to Google (privacy)
- Can't store 500K URLs locally on mobile
- Must be instant (can't block page loads)

**Solution:** Download Bloom filter of malicious URLs.

```python
class SafeBrowsing:
    def check_url(self, url):
        # Step 1: Local filter check (instant, 99.9% of URLs)
        if url not in self.malicious_urls_filter:
            return "SAFE"

        # Step 2: Network check for rare matches (1% FP)
        return self.check_with_server(url)  # Definitive answer
```

**Numbers:**
- Filter size: 500K URLs × 10 bits = ~625 KB
- vs. Full URL database: 500K × 100 bytes = 50 MB
- Savings: 98%
- Server requests: 1% of URLs vs 100% (99% reduction)

---

### C. Content Recommendations (Medium, Netflix)

**Problem:** Show users articles they haven't read. Each user might have read 10K+ articles.

**Naive approach:**
```
10M users × 10K articles × 64 bits/item = 6.4 TB memory
```

**With Bloom filter:**
```
10M users × 10K articles × 14.4 bits/item = 180 GB memory
# 0.1% FP rate chosen to minimize false recommendations
```

**Savings:** 97% memory reduction. Acceptable: 1 in 1000 recommendations might be already-read articles.

---

### D. Cache Efficiency (Akamai CDN, Facebook Memcache)

**Problem:** Edge servers cache content. Need to quickly check if content is cached before forwarding to origin.

```python
class EdgeServer:
    def get(self, url):
        # Fast check: Is it definitely NOT cached?
        if url not in self.cache_filter:
            return self.fetch_from_origin(url)

        # Might be cached - check actual cache
        result = self.cache.get(url)
        if result:
            return result

        # False positive - not actually cached
        return self.fetch_from_origin(url)
```

**Impact:** Avoid expensive hash table lookups for 99% of cache misses.

---

### E. Distributed Request Deduplication (Squid Proxy)

**Problem:** Multiple clients request the same resource simultaneously. Only want to fetch from origin once.

```python
class Proxy:
    def get(self, url):
        # Fast check: Is request already in-flight?
        if url not in self.in_flight_filter:
            # Definitely not in-flight - start new request
            return self.start_request(url)

        # Might be in-flight - check actual dictionary
        if url in self.pending_requests:
            return self.pending_requests[url].wait()

        # False positive - not actually in-flight
        return self.start_request(url)
```

**Impact:** 10 simultaneous requests → 1 origin request.

---

### F. Bitcoin Lightweight Wallets (BIP 37)

**Problem:** SPV (Simplified Payment Verification) wallets need to check if transactions are relevant without downloading the entire blockchain (4+ GB).

```python
class SPVWallet:
    def __init__(self):
        # Track only our addresses
        self.filter = BloomFilter.create_optimal(1000, 0.001)
        for address in self.my_addresses:
            self.filter.add(address)

    def send_to_node(self):
        # Full node only sends transactions matching this filter
        return self.filter
```

**Impact:**
- Mobile wallet memory: 1 KB filter vs 4 GB UTXO set
- Bandwidth: Only receive relevant transactions + 0.1% false positives

---

### G. Rate Limiting and Attack Detection (Cloudflare DDoS)

**Problem:** Track request patterns from millions of IPs without storing all IP addresses.

```python
class DDoSDetector:
    def __init__(self):
        self.current_minute = BloomFilter.create_optimal(1_000_000, 0.01)
        self.counts = {}

    def record_request(self, ip, timestamp):
        if ip not in self.current_minute:
            self.current_minute.add(ip)
            self.counts[ip] = 1
        else:
            self.counts[ip] = self.counts.get(ip, 0) + 1

        if self.counts.get(ip, 0) > 100:
            self.block_ip(ip)

    def rotate_window(self):
        # Called every minute - discard old filter
        self.current_minute = BloomFilter.create_optimal(1_000_000, 0.01)
        self.counts = {}
```

**Impact:** Memory: 10 MB vs 64 MB for storing all IPs.

---

## 4. Bloom Filters in Distributed Systems

### Challenge 1: Locating Data Across Nodes

In a distributed system with 1000 nodes:
- Query: "Does key X exist?"
- Naive answer: Query all 1000 nodes (~50 seconds at 50ms per query)
- **Bloom filter solution:** Use filters to eliminate candidates (500x speedup)

### Pattern A: Local Filters Per Node

Each node maintains a Bloom filter of its own data:

```python
# Node 1
node1.bloom_filter.add("user:12345")
node1.data = {"user:12345": {...}}

# Node 2
node2.bloom_filter.add("user:111")
node2.data = {"user:111": {...}}

# Query routing
def find_key(key):
    for node in nodes:
        if key in node.bloom_filter:  # O(k) = microseconds
            result = node.remote_get(key)  # 50ms network call
            if result:
                return result
    return None

# Performance:
# - Without filter: Query 1000 nodes = 50 seconds
# - With filter (1% FP): Query ~2 nodes = 100ms
# - Improvement: 500x faster
```

**Advantages:**
- Filters are tiny (kilobytes)
- Distributed - no single point of failure
- Each node only knows about its own data

---

### Pattern B: Centralized Filter Registry

Coordinator maintains filters for all nodes:

```python
class Coordinator:
    def __init__(self, nodes):
        self.node_filters = {
            node_id: BloomFilter.create_optimal(100_000, 0.01)
            for node_id in nodes
        }

    def route_query(self, key):
        # Check all filters locally (fast)
        candidates = []
        for node_id, bf in self.node_filters.items():
            if key in bf:
                candidates.append(node_id)

        # Query only candidate nodes
        for node_id in candidates:
            result = self.nodes[node_id].get(key)
            if result:
                return result
        return None
```

**Memory calculation:**
```
1000 nodes × 100K items/node × 9.6 bits/item = 117 MB
vs
1000 nodes × 100K items/node × 64 bits/item (hash set) = 75 GB

Savings: 99.8%
```

**Trade-offs:**
- ✅ Single location - easy to update
- ❌ Single point of failure

---

### Pattern C: Hierarchical Filters

Multi-level routing reduces false positive amplification:

```python
# Level 1: Which datacenter?
dc_filters = {
    "us-east": BloomFilter(...),
    "eu-west": BloomFilter(...),
}

# Level 2: Which rack in datacenter?
rack_filters = {...}

# Level 3: Which server in rack?
server_filters = {...}

def hierarchical_query(key):
    # Level 1: Check datacenters (3 filters)
    for dc, bf in dc_filters.items():
        if key not in bf:
            continue  # Skip entire DC

        # Level 2: Check racks (10 filters)
        for rack, bf in rack_filters[dc].items():
            if key not in bf:
                continue  # Skip entire rack

            # Level 3: Check servers (10 filters)
            for server, bf in server_filters[rack].items():
                if key in bf:
                    result = query_server(server, key)
                    if result:
                        return result
    return None

# Expected queries: ~3 (1 DC + 1 rack + 1 server)
# vs 1000 queries without filters
```

**False positive amplification:**
```
Single filter: 1% FP
Three levels: 1 - (0.99)³ = 2.97% FP (acceptable)
```

---

### Challenge 2: Consistency and Updates

**Problem:** How to keep filters consistent when data changes?

**Solution: Broadcast Updates**

```python
class ConsistentBloomFilter:
    def add(self, key):
        # Write to data store
        self.data[key] = value

        # Update local filter
        self.filter.add(key)

        # Broadcast to peers (async)
        for peer in self.peers:
            peer.filter_add(key)  # Async message
```

**Challenges:**
- Network partitions → inconsistent filters
- Message loss → missing updates
- Solution: Periodic full rebuild from source data

---

### Challenge 3: Filter Capacity Planning

**Problem:** Nodes grow at different rates. Filter designed for 100K items, now has 500K.

```python
# Initial: FP = 1%
# After 5x growth: FP = 80% (DEGRADED!)

# Solution: Dynamic resizing
def maybe_resize_filter(node):
    if node.filter.get_fill_ratio() > 0.7:
        old_filter = node.filter
        new_filter = BloomFilter.create_optimal(
            len(node.data) * 1.5,  # 50% headroom
            fp_rate=0.01
        )

        # Rebuild from actual data
        for key in node.data.keys():
            new_filter.add(key)

        node.filter = new_filter
```

---

### Challenge 4: Handling Deletions

**Problem:** Standard Bloom filters don't support deletion. If you delete data, filter still says it exists.

**Solution 1: Counting Bloom Filter**
```python
# Use 4-bit counters instead of 1-bit flags
# Increment on add, decrement on delete
# Trade-off: 4x memory overhead
```

**Solution 2: Periodic Rebuild**
```python
def rebuild_filter_from_data():
    new_filter = BloomFilter.create_optimal(len(node.data), 0.01)
    for key in node.data.keys():
        new_filter.add(key)
    node.filter = new_filter

# Run every hour/day depending on delete rate
```

**Solution 3: Time-Windowed Filters**
```python
class TimeWindowedFilter:
    def __init__(self):
        self.current_hour = BloomFilter(...)
        self.last_hour = BloomFilter(...)

    def rotate(self):
        """Called every hour"""
        self.last_hour = self.current_hour
        self.current_hour = BloomFilter(...)  # Fresh filter

    def contains(self, key):
        return (key in self.current_hour or
                key in self.last_hour)
```

---

### Challenge 5: Network Overhead

**Problem:** Sending entire filter (1-100 MB) on every update is expensive.

**Solution: Incremental Updates**
```python
def send_filter_delta(old_version, new_version):
    """Send only changed bits since last version"""
    delta = {
        'added_keys': keys_added_since(old_version),
        'version': new_version
    }
    return delta  # Much smaller than entire filter

# Better: Periodic full sync + deltas
if time_since_last_full_sync() < 300:  # 5 minutes
    send_delta()  # Small (kilobytes)
else:
    send_full_filter()  # Rebuild to avoid drift
```

---

### Challenge 6: Server Failures

**Problem:** Node goes down. Its filter disappears. Other nodes can't route to it.

**Solutions:**

1. **Replication:**
   - Keep copies of filters on backup nodes
   - If primary fails, use replica

2. **Rebuild from Data:**
   - Store source data persistently
   - On node restart, rebuild filter from data
   - Takes time but ensures consistency

3. **Accept Temporary Routing Failures:**
   - Old filters have stale data
   - New queries temporarily go to wrong nodes
   - Self-healing as filters get updated

---

## 5. When NOT to Use Bloom Filters

### ❌ Anti-Pattern 1: Small Datasets (< 1000 elements)

**Why it seems good:** "Bloom filters are memory efficient!"

**Why it fails:** Overhead outweighs benefits.

```python
# Bloom filter for 100 items
bf = BloomFilter.create_optimal(100, 0.01)
# Size: 120 bytes
# Operations: 7 hash functions per operation

# Hash set for 100 items
hs = set()
# Size: ~6.4 KB
# Operations: 1 hash function per operation

# Hash set wins on both speed AND space
# Difference is negligible on modern hardware
```

**Rule:** Only use Bloom filters when savings > 10 KB or dataset > 10,000 elements.

---

### ❌ Anti-Pattern 2: Deletions Required

**Why it seems good:** "Bloom filters are efficient and I need to track a dynamic set."

**Why it fails:** Can't delete items from standard Bloom filter.

```python
cache.remove(url)
# But filter still says url is cached (false positive)
# Over time: FP rate degrades from 1% to 50%+
```

**Solutions:**
- Use Counting Bloom Filter (4x memory overhead)
- Use Cuckoo Filter (supports deletions, same space efficiency)
- Periodic rebuild from source data
- Use hash set if memory allows

---

### ❌ Anti-Pattern 3: False Positives Unacceptable

**Why it seems good:** "I need to filter out bad data quickly."

**Why it fails:** False positives are inherent.

```python
# Security example: Spam filter
class SpamFilter:
    def __init__(self):
        self.spam_ips = BloomFilter.create_optimal(100_000, 0.01)

    def is_spam(self, ip):
        if ip in self.spam_ips:
            return True  # Block access
        return False

# 1% FP rate means 1% of legitimate IPs get blocked!
# 1M daily users × 1% = 10,000 legitimate users blocked
# Support team overwhelmed
```

**Rule:** Never use Bloom filters as the ONLY decision maker for critical operations.

**Better approach:** Two-phase validation
```python
# Phase 1: Bloom filter (fast, pre-filter)
if ip not in bloom_filter:
    return "ALLOWED"  # Definitely safe

# Phase 2: Database lookup (slow, authoritative)
if database.is_spam(ip):
    return "BLOCKED"  # Confirmed spam

return "ALLOWED"  # False positive from Bloom filter
```

---

### ❌ Anti-Pattern 4: Need to Enumerate Elements

**Why it seems good:** "I need to store and query a large set."

**Why it fails:** Can't list what's in the filter.

```python
class UserFollowers:
    def __init__(self):
        self.followers = BloomFilter.create_optimal(100_000, 0.01)

    def get_all_followers(self):
        # IMPOSSIBLE!
        # Bloom filter doesn't store actual elements
        return ???
```

**Rule:** If you need to list, export, or display elements, use a different data structure.

---

### ❌ Anti-Pattern 5: False Positive Cost Too High

**Why it seems good:** "1% false positives seems small."

**Why it fails:** When FP cost is high, savings aren't worth it.

```python
# Web crawler: track visited URLs
class WebCrawler:
    def crawl(self, url):
        if url in self.visited_filter:  # 1% FP
            return  # Skip (or false positive skip)

        self.visited.add(url)
        page = self.fetch(url)  # Expensive: 200ms

# Analysis:
# URLs to crawl: 10M
# False positives: 100K URLs incorrectly skipped
# Result: Missing 100K pages from search index
# Cost: SEO damage, incomplete search results

# Memory saved: 68 MB
# Value of 100K pages: Priceless
```

**Rule:** Calculate if FP_cost × FP_rate > memory_saved. If so, don't use Bloom filter.

---

### ❌ Anti-Pattern 6: Exact Count Required

**Why it seems good:** "I need to count unique visitors."

**Why it fails:** False positives corrupt the count.

```python
def record_visit(user_id):
    if user_id not in self.filter:
        self.filter.add(user_id)
        self.count += 1  # WRONG

# Sequence:
# user_1 visits → not in filter → count=1 ✓
# user_2 visits → not in filter → count=2 ✓
# user_3 visits → false positive → count=2 ✗

# Actual unique: 3
# Reported count: 2 (error: -33%)
```

**Better solutions:**
- HyperLogLog (designed for cardinality estimation)
- Hash set (for exact count)
- Database with DISTINCT

---

### ❌ Anti-Pattern 7: Highly Dynamic Workload

**Why it seems good:** "Dataset is growing, and Bloom filters are fast."

**Why it fails:** Filter capacity is fixed. As dataset grows beyond capacity, FP rate degrades exponentially.

```python
# Designed for 100K items @ 1% FP
# After 6 months: 500K items

# New FP rate: 82% (from 1%)
# Filter is now useless

# Solution: Resize
# But can't enumerate elements in old filter!
# Must rebuild from source data (expensive)
```

**Rule:** If dataset size is unpredictable, over-provision 3x or use dynamic structures (Cuckoo filter, cascading Bloom filters).

---

### ❌ Anti-Pattern 8: Exact Matching Required

**Why it seems good:** "I need to check permissions exactly."

**Why it fails:** False positives grant access to non-authorized users.

```python
class AccessControl:
    def __init__(self):
        self.allowed_users = BloomFilter.create_optimal(1_000_000, 0.01)

    def is_allowed(self, user):
        return user in self.allowed_users  # 1% FP = 1% wrong grants!
```

**Rule:** Use exact data structures (hash set, database) for security-critical operations.

---

## Summary: Bloom Filter Decision Framework

### Ask These Questions Before Using a Bloom Filter:

1. **Dataset size:** > 10K elements? (No → use hash set)
2. **Deletions:** Are deletions rare? (No → use Cuckoo filter)
3. **False positives:** Can you tolerate 0.1-1% FPs? (No → use exact structure)
4. **FP cost:** Is FP_cost × FP_rate < memory_saved? (No → don't use)
5. **Enumeration:** Do you need to list elements? (Yes → don't use)
6. **Exact counts:** Do you need exact counts? (Yes → use HyperLogLog or exact)
7. **Growth:** Is dataset size predictable? (No → over-provision 3x)
8. **Criticality:** Is this a critical operation? (Yes → use exact structure or two-phase)

**If you answer "no" to any critical question, don't use a Bloom filter.**

---

## Production Implementation

### Creating Optimal Filters

```python
# For 1M items with 1% false positive rate
bf = BloomFilter.create_optimal(
    expected_elements=1_000_000,
    fp_rate=0.01
)

# Size: 1M × 9.6 bits = 9.6 Mb ≈ 1.2 MB
# Hash functions: 7
# Time complexity: O(7) per operation
```

### Monitoring

```python
# Monitor fill ratio
if bf.get_fill_ratio() > 0.7:
    alert("Bloom filter over-capacity")

# Monitor actual FP rate
actual_fp = bf.estimate_false_positive_rate()
if actual_fp > target_fp * 2:
    alert("FP rate degrading")
```

### Persistence

**Persist to disk if:**
- ✅ Rebuild time > 1 minute
- ✅ Source data is on disk (database, large files)
- ✅ Zero downtime required on restart

**Keep memory-only if:**
- ✅ Rebuild time < 10 seconds
- ✅ Source data is in memory (cache, sessions)
- ✅ Small filter (< 1 MB)

---

## References

1. [Bloom, B. H. (1970). "Space/Time Trade-offs in Hash Coding with Allowable Errors"](https://citeseerx.ist.psu.edu/viewdoc/summary?doi=10.1.1.641.9096)
2. [Kirsch & Mitzenmacher (2006). "Less Hashing, Same Performance"](https://www.eecs.harvard.edu/~michaelm/postscripts/tr-02-05.pdf)
3. [Network Applications of Bloom Filters (Survey)](https://www.eecs.harvard.edu/~michaelm/postscripts/im2005b.pdf)

---

**Key Takeaways:**
- Understand the probabilistic guarantees (zero false negatives, controlled false positives)
- Know the math: P(FP) = (1 - e^(-kn/m))^k
- Recognize when space savings matter (10K+ elements, expensive operations to avoid)
- Identify when NOT to use (deletions, zero FP tolerance, critical operations)
- In distributed systems: use filters to reduce network calls and enable fast local decisions about remote data
