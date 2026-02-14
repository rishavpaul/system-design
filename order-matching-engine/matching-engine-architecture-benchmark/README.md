# Why Single-Threaded Matching Engines Beat Multi-Threaded Designs

## The Surprising Result

You'd intuitively think: "8 CPU cores with 8 threads = 8x faster than 1 thread."

**Reality:** LMAX Exchange processes **6 million stock orders per second** using **one thread per stock**. Their old multi-threaded system? About **750,000 orders per second** using a thread pool.

**Single-threaded wins by 8x.**

Source: [LMAX Disruptor Performance Results](https://github.com/LMAX-Exchange/disruptor/wiki/Performance-Results)

This document explains why, step by step, with real data.

---

## Part 0: Fundamentals - How CPUs and Memory Really Work

Before we compare architectures, we need to understand how modern CPUs actually execute programs. This section covers the essential concepts that most developers learn superficially but rarely master.

### 0.1 CPU Speed: Cycles, Gigahertz, and Nanoseconds

**What is a CPU cycle?**

A CPU cycle is one "tick" of the CPU's internal clock. Think of it like a heartbeat for your CPU - everything happens in discrete steps synchronized to this clock.

**What are "basic operations" (instructions)?**

A CPU executes **instructions** - simple commands written in machine code. Each instruction is a basic operation like:

**Arithmetic instructions:**
- `ADD r1, r2, r3` - Add registers r2 and r3, store result in r1
- `SUB r1, r2, r3` - Subtract r3 from r2
- `MUL r1, r2, r3` - Multiply
- `DIV r1, r2, r3` - Divide

**Memory instructions:**
- `LOAD r1, [address]` - Load value from memory into register r1
- `STORE [address], r1` - Write register r1 to memory

**Control flow instructions:**
- `JUMP label` - Jump to another location in code
- `BRANCH_IF_ZERO r1, label` - Conditional jump
- `CALL function` - Call a function
- `RETURN` - Return from function

**Comparison instructions:**
- `CMP r1, r2` - Compare two values
- `TEST r1, r2` - Bitwise test

**Bitwise/Logical instructions:**
- `AND r1, r2, r3` - Bitwise AND
- `OR r1, r2, r3` - Bitwise OR
- `XOR r1, r2, r3` - Bitwise XOR
- `SHIFT_LEFT r1, 2` - Shift bits left

**Your high-level code:**
```c
int c = a + b;
```

**Becomes machine instructions:**
```asm
LOAD r1, [memory_address_of_a]    ; Load 'a' into register r1
LOAD r2, [memory_address_of_b]    ; Load 'b' into register r2
ADD r3, r1, r2                    ; Add r1 and r2, store in r3
STORE [memory_address_of_c], r3   ; Write result to 'c'
```

That's **4 instructions** for one line of C code!

**How many instructions per cycle?**

**Old CPUs (1980s-1990s):** 1 instruction per cycle (or less)

**Modern CPUs (2000s+):** Multiple instructions per cycle through:

1. **Pipelining** - Like an assembly line:
   ```
   Cycle 1: [Fetch Inst1]
   Cycle 2: [Decode Inst1] [Fetch Inst2]
   Cycle 3: [Execute Inst1] [Decode Inst2] [Fetch Inst3]
   Cycle 4: [Write Inst1] [Execute Inst2] [Decode Inst3] [Fetch Inst4]
   ```
   Four instructions are in progress simultaneously!

2. **Superscalar execution** - Multiple execution units:
   ```
   ┌─────────────────┐
   │  Fetch/Decode   │
   └────────┬────────┘
            │
      ┌─────┴─────┐
      ↓           ↓
   [ALU 1]     [ALU 2]     ← Two arithmetic units
      ↓           ↓
   [Load/Store Unit]       ← Memory unit
      ↓
   [Branch Unit]           ← Jump/branch unit
   ```

   Can execute 4+ instructions per cycle if they don't depend on each other!

3. **Out-of-order execution** - CPU reorders instructions for efficiency:
   ```c
   a = b + c;     // Instruction 1
   d = e + f;     // Instruction 2 (independent!)
   g = a + 1;     // Instruction 3 (depends on 1)
   ```

   CPU might execute: Inst1, Inst2 (in parallel), then Inst3
   Even though you wrote them in order!

**IPC (Instructions Per Cycle):**

Modern CPUs achieve **2-4 IPC** on average (some workloads hit 6+ IPC).

So a **3.0 GHz CPU** with **3 IPC**:
- 3 billion cycles/second × 3 instructions/cycle = **9 billion instructions/second**

**But here's the catch:**

If those instructions need data from RAM:
- `LOAD` from RAM: ~100 nanoseconds = ~300 cycles wasted
- CPU executes 0 instructions while waiting!
- **Actual IPC drops to < 0.5** (CPU spends most time idle)

**This is why cache matters so much** - it keeps the CPU fed with data so it can maintain high IPC.

**What is GHz (Gigahertz)?**

A CPU's clock speed, measured in GHz, tells you how many cycles happen per second.

- **2.5 GHz** = 2.5 billion cycles per second
- **3.0 GHz** = 3.0 billion cycles per second

**Converting cycles to real time:**

If you have a **2.5 GHz CPU**:
- 1 cycle = 1 / 2,500,000,000 seconds = **0.4 nanoseconds**

So when we say "L1 cache access takes 4 cycles":
- 4 cycles × 0.4 ns/cycle = **1.6 nanoseconds**

**Time units you'll see:**
- **1 second** = 1,000,000,000 nanoseconds (1 billion ns)
- **1 millisecond (ms)** = 1,000,000 nanoseconds
- **1 microsecond (μs)** = 1,000 nanoseconds
- **1 nanosecond (ns)** = base unit

**Why this matters:**

Modern CPUs can execute billions of instructions per second, but if each instruction has to wait 100 nanoseconds for data from RAM, you're wasting those billions of cycles waiting. This is the core problem we're solving.

### 0.2 The Memory Hierarchy: Why Cache Exists

**The fundamental problem:**

CPUs are **incredibly fast** at computation. A modern 3 GHz CPU can theoretically execute 3 billion operations per second.

But there's a catch: **the CPU can only work on data that's in its registers** (tiny, super-fast storage directly on the CPU chip).

**Where does data come from?**

Your program's data (variables, objects, arrays) lives in **RAM** (Random Access Memory). RAM is:
- **Large** (8-128 GB typically)
- **Off the CPU chip** (centimeters away)
- **Slow** (100+ nanoseconds to fetch)

**The bottleneck:**

If the CPU had to fetch every piece of data from RAM:
- Each fetch: ~100 nanoseconds
- At 3 GHz: CPU could do 300 operations in that time!
- **CPU sits idle 99% of the time waiting for data**

**The solution: Cache hierarchy**

Engineers added small, fast memory between the CPU and RAM:

```
CPU Registers (100 bytes)           ← Smallest, fastest
    ↓
L1 Cache (32-64 KB per core)        ← Very small, very fast
    ↓
L2 Cache (256-512 KB per core)      ← Small, fast
    ↓
L3 Cache (8-32 MB, shared)          ← Medium, pretty fast
    ↓
RAM (8-128 GB)                      ← Huge, slow
    ↓
SSD/Disk (512 GB - 4 TB)           ← Enormous, very slow
```

**The tradeoff: Speed vs Capacity**

| Memory Level | Size | Latency (cycles) | Latency (ns @ 2.5GHz) | Cost Reason |
|--------------|------|------------------|----------------------|-------------|
| Registers | ~100 bytes | 1 | 0.4 | On-die, part of CPU |
| L1 Cache | 32 KB | 4 | 1.6 | On-die, SRAM |
| L2 Cache | 256 KB | 12 | 5 | On-die, SRAM |
| L3 Cache | 8 MB | 40 | 16 | On-die, SRAM |
| RAM | 16 GB | 200 | 80 | Off-chip, DRAM |

Source: [Cache Memory Latency Benchmarks](https://medium.com/applied/applied-c-memory-latency-d05a42fe354e)

**Why can't we just make L1 cache huge?**

**Physics and economics:**
- L1 cache uses **SRAM** (Static RAM) - very fast but expensive and power-hungry
- Distance matters: L1 is physically closer to the CPU core (~millimeters)
- Larger caches = longer wire distances = slower access times
- Cost: SRAM costs ~1000x more per byte than DRAM (RAM)

**The key insight:**

Most programs exhibit **locality of reference**:
- **Temporal locality**: If you access data once, you'll likely access it again soon
- **Spatial locality**: If you access data at address X, you'll likely access X+1, X+2 soon

Caches exploit this: keep recently-used data close to the CPU.

### 0.3 Cache Mechanics: Lines, Hits, and Misses

**What is a cache line?**

Caches don't move data one byte at a time. They move data in fixed-size chunks called **cache lines**.

**On x86 CPUs: 1 cache line = 64 bytes**

**Why 64 bytes?**

Spatial locality. If your program reads `array[0]`, it will probably read `array[1]`, `array[2]`, etc. soon.

So instead of fetching just 1 byte, the CPU fetches the entire surrounding 64-byte block.

**Example:**

```c
int array[16];  // 16 integers × 4 bytes = 64 bytes = 1 cache line

int x = array[0];  // Cache miss: fetch entire 64-byte line from RAM (100ns)
int y = array[1];  // Cache HIT: already in cache (2ns)
int z = array[2];  // Cache HIT: already in cache (2ns)
```

**Cache hit vs cache miss:**

- **Cache hit**: Data is already in cache → fast (2-5 ns)
- **Cache miss**: Data not in cache → must fetch from next level (12 ns - 100 ns)

**Cache hit rate:**

If 90% of accesses are cache hits:
- 90% × 2ns = 1.8ns (fast)
- 10% × 100ns = 10ns (slow)
- **Average: 11.8ns**

If 99% of accesses are cache hits:
- 99% × 2ns = 1.98ns
- 1% × 100ns = 1ns
- **Average: 2.98ns**

**Going from 90% to 99% hit rate makes you 4x faster!**

This is why single-threaded wins: it achieves 99%+ cache hit rates vs 70-90% for multi-threaded.

**What causes a cache miss?**

1. **First access** (compulsory miss): Data never loaded before
2. **Capacity miss**: Cache is full, had to evict data
3. **Conflict miss**: Hash collision in cache organization
4. **Coherency miss**: Another core modified the data (invalidated your copy)

For multi-threaded matching engines, #4 (coherency miss) is the killer.

### 0.4 Multi-Core CPUs: Parallelism and Its Costs

**What is a CPU core?**

A core is an independent execution unit that can run instructions.

Modern CPUs have multiple cores:
- Desktop: 4-16 cores
- Server: 32-128 cores
- Apple M3 Max: 16 cores

**What is a thread?**

A thread is a sequence of instructions that the OS schedules on a CPU core.

- 1 core can run 1 thread at a time (or 2 with hyperthreading, but that's different)
- Multiple threads = multiple cores working in parallel

**Cache architecture in multi-core CPUs:**

Each core has **private** L1 and L2 caches:

```
┌─────────────────────────────────────────────────────┐
│ CPU Package                                         │
│                                                     │
│  ┌──────────────┐         ┌──────────────┐        │
│  │   Core 1     │         │   Core 2     │        │
│  │              │         │              │        │
│  │  L1: 32 KB   │         │  L1: 32 KB   │        │ ← Private per core
│  │  L2: 256 KB  │         │  L2: 256 KB  │        │ ← Private per core
│  └──────┬───────┘         └──────┬───────┘        │
│         │                        │                │
│         └────────────┬───────────┘                │
│                      │                            │
│         ┌────────────┴────────────┐               │
│         │   L3 Cache: 16 MB       │               │ ← Shared across cores
│         └────────────┬────────────┘               │
└──────────────────────┼─────────────────────────────┘
                       │
              ┌────────┴────────┐
              │   Main Memory   │
              │   (RAM: 16 GB)  │
              └─────────────────┘
```

**The problem with separate caches:**

If Core 1 and Core 2 both need the same data (e.g., an order book):
- Core 1 loads it into its L1 cache
- Core 2 must ALSO load it into its L1 cache
- Now two copies exist!

**What if Core 1 modifies the data?**
- Core 2's copy is now stale (out of date)
- Core 2 must be notified to invalidate its copy
- This requires **cache coherency protocols**

### 0.5 Cache Coherency: The Hidden Cost of Sharing

**The problem:**

```
Core 1's L1 cache: [Order Book v1]
Core 2's L1 cache: [Order Book v1]

Core 1 modifies order book → [Order Book v2]

Core 2's cache now has stale data!
```

**The solution: MESI protocol**

MESI (Modified, Exclusive, Shared, Invalid) is a protocol that keeps caches synchronized.

**Each cache line is in one of 4 states:**

- **Modified (M)**: This core modified it, no other core has it → "I own this exclusively and changed it"
- **Exclusive (E)**: Only this core has it, not modified → "I own this exclusively but haven't changed it"
- **Shared (S)**: Multiple cores have read-only copies → "Others might have this too"
- **Invalid (I)**: Cache line is stale, must re-fetch → "My copy is garbage"

**What happens when Core 1 modifies data:**

```
Initial state:
  Core 1: [Order Book] state = Shared
  Core 2: [Order Book] state = Shared

Core 1 writes to order book:
  1. Core 1 sends "Invalidate" message to ALL other cores
  2. Core 2 marks its cache line as Invalid
  3. Core 1 marks its line as Modified

  Core 1: [Order Book v2] state = Modified
  Core 2: [Order Book]    state = Invalid (stale!)

Core 2 tries to read order book:
  1. Cache miss (state = Invalid)
  2. Sends request to Core 1
  3. Core 1 sends cache line to Core 2 (and to RAM)
  4. Now both have Shared copies

  Core 1: [Order Book v2] state = Shared
  Core 2: [Order Book v2] state = Shared
```

**The cost:**

Every write causes:
1. **Invalidation messages** sent across cores (~20-40 cycles)
2. **Cache line transfers** between cores (~50 ns)
3. **Next access is a cache miss** on other cores (~100 ns)

Source: [Cache Coherence: MESI Protocol](https://medium.com/codetodeploy/cache-coherence-how-the-mesi-protocol-keeps-multi-core-cpus-consistent-a572fbdff5d2)

**False sharing:**

Even if two threads access **different variables**, if those variables are on the same 64-byte cache line, writes from one thread invalidate the other thread's cache.

```c
struct {
    int threadA_counter;  // Byte 0-3
    int threadB_counter;  // Byte 4-7  ← Same cache line!
} shared_data;

// Thread A (Core 1)
shared_data.threadA_counter++;  // Invalidates Core 2's cache line

// Thread B (Core 2)
shared_data.threadB_counter++;  // Cache miss! Even though it's a different variable!
```

**This is why padding matters:**

```c
struct {
    int threadA_counter;
    char padding[60];      // Pad to 64 bytes
    int threadB_counter;   // Now on separate cache line!
} shared_data;
```

### 0.6 Operating System Overhead: Context Switches and Syscalls

**What is a context switch?**

When the OS switches from running Thread A to Thread B on the same CPU core, it must:

1. **Save Thread A's state:**
   - All CPU registers (16-32 registers)
   - Program counter (where Thread A was in code)
   - Stack pointer

2. **Load Thread B's state:**
   - Restore Thread B's registers
   - Restore program counter
   - Switch to Thread B's stack

3. **Side effects:**
   - **TLB flush**: Translation Lookaside Buffer (virtual→physical address cache) is invalidated
   - **Cache pollution**: Thread B's data replaces Thread A's data in cache
   - **Pipeline flush**: CPU's instruction pipeline must be cleared

**Cost: 3,000-5,000 nanoseconds** (3-5 microseconds)

Source: [Context Switch Cost Benchmarks](https://blog.tsunanet.net/2010/11/how-long-does-it-take-to-make-context.html)

At 2.5 GHz, that's 7,500-12,500 cycles of wasted work!

**What is a syscall (system call)?**

When your program needs the operating system to do something (allocate memory, open file, wait on lock), it makes a syscall:

1. **Switch from user mode to kernel mode** (privilege escalation)
2. **Execute kernel code**
3. **Switch back to user mode**

**Cost: ~500-1,000 nanoseconds** for simple syscalls

**What is a futex?**

A futex (fast userspace mutex) is Linux's locking mechanism.

**Uncontended path (fast):**
```c
// Just an atomic compare-and-swap in userspace
if (atomic_CAS(&lock, 0, 1)) {
    // Got lock! No syscall needed
    // Cost: ~25-50 nanoseconds
}
```

**Contended path (slow):**
```c
// Lock is held by another thread
if (!atomic_CAS(&lock, 0, 1)) {
    // Call kernel to put this thread to sleep
    futex_wait(&lock);  // Syscall! Cost: ~3,000-5,000 ns
}
```

Source: [Basics of Futexes](https://eli.thegreenplace.net/2018/basics-of-futexes/)

**Why contention is so expensive:**

When Thread B can't get the lock:
1. Syscall to kernel (~500 ns)
2. Kernel puts Thread B to sleep (context switch: ~3,000 ns)
3. When lock is released, kernel wakes Thread B (another context switch: ~3,000 ns)

**Total: ~6,000-7,000 nanoseconds** vs 25 ns uncontended!

**What is CPU affinity (thread pinning)?**

By default, the OS scheduler can move threads between CPU cores for load balancing.

**CPU affinity** lets you pin a thread to a specific core:

```c
// Linux example
cpu_set_t cpuset;
CPU_ZERO(&cpuset);
CPU_SET(2, &cpuset);  // Pin to Core 2
pthread_setaffinity_np(thread, sizeof(cpuset), &cpuset);
```

**Why this matters:**

If a thread keeps moving between cores:
- Cache is cold on the new core (data must be re-fetched)
- TLB must be rebuilt
- Performance is unpredictable

If a thread is pinned:
- Cache stays hot (data stays in that core's L1/L2)
- Consistent, predictable performance

### 0.7 Performance Dimensions: What We Actually Measure

When evaluating system performance, we measure multiple dimensions:

**1. Throughput (Operations per Second)**

How many operations can the system complete per second?

- **Orders/second**: 1,000,000 orders/sec
- **Requests/second**: 50,000 req/sec
- **Trades/second**: 500,000 trades/sec

Higher is better, but doesn't tell the full story.

**2. Latency (Time per Operation)**

How long does each operation take?

**Metrics:**
- **Mean latency**: Average time (can be misleading)
- **Median (p50)**: 50% of operations are faster than this
- **p99 latency**: 99% of operations are faster than this (1% are slower)
- **p999 latency**: 99.9% faster than this (0.1% are slower)

**Why tail latency (p99, p999) matters:**

If mean latency is 100μs but p99 is 10ms:
- 99% of customers have good experience
- 1% experience 100x worse latency (feels broken to them!)

For trading systems, **p99 and p999 matter more than mean**.

**3. CPU Utilization**

What percentage of CPU time is spent doing useful work vs waiting?

- **High utilization (90-100%)**: CPU is busy computing (good for batch processing)
- **Low utilization (10-30%)**: CPU is waiting (blocked on locks, I/O, etc.)

For latency-critical systems, you **want** low utilization (headroom for bursts).

**4. Cache Hit Rate**

Percentage of memory accesses served from cache vs RAM:

- **99% hit rate**: 99% from L1 cache (2ns), 1% from RAM (100ns) → avg 3ns
- **90% hit rate**: 90% from L1 cache (2ns), 10% from RAM (100ns) → avg 12ns

**10% worse hit rate = 4x slower memory access!**

Measured with tools like `perf`:
```bash
perf stat -e cache-references,cache-misses ./program
```

**5. Lock Contention**

How often do threads wait for locks?

- **Contention rate**: Percentage of lock acquisitions that block
- **Wait time**: Average time spent waiting for locks

Measured with profiling tools or instrumentation.

**6. Context Switches**

How often does the OS switch between threads?

```bash
perf stat -e context-switches ./program
```

**Red flag:** >1,000 context switches/second per thread indicates thrashing.

**7. Scalability**

How does performance change with load?

- **Linear scalability**: 2x cores = 2x throughput (ideal)
- **Sub-linear**: 2x cores = 1.5x throughput (common with locks)
- **Negative**: 2x cores = 0.5x throughput (severe contention)

**Why multi-threaded matching engines show negative scalability:**

Adding more threads increases lock contention, cache coherency overhead, and context switches faster than it adds compute capacity.

---

**Now that we understand the fundamentals, let's see how they apply to matching engines...**

---

## Part 1: The Library Analogy (Building Intuition)

Before diving into CPUs and caches, let's build intuition with a library analogy.

### Traditional Multi-Threaded Approach

**Setup:**
- 5 librarians work together
- All books are in one locked room
- Only 1 librarian can be in the room at a time (they need the key)
- Any librarian can help with any book

**Customer arrives asking for "Harry Potter":**

```
Librarian 1: "I'll get it!" → Takes key → Enters room → Searches for book
Librarian 2: "I can help!" → Waits for key... (standing outside)
Librarian 3: "Me too!" → Waits for key... (standing outside)
Librarian 4: Also waiting...
Librarian 5: Also waiting...
```

**Problems:**
1. **Only 1 librarian works at a time** (even though you have 5!)
2. **Time wasted waiting for the key** (lock contention)
3. **Nobody remembers where books are** because different librarians move books around

### Single-Threaded Approach

**Setup:**
- 5 librarians, each with their own dedicated section
- Librarian 1: Fiction A-F (has their own room, no lock needed)
- Librarian 2: Fiction G-M (has their own room)
- Librarian 3: Fiction N-Z (has their own room)
- etc.

**Customer arrives asking for "Harry Potter":**

```
Customer → Directed to Librarian 1 (H is in A-F section)
Librarian 1: Instantly knows where it is → Retrieves it
Total wait time: Minimal
```

Meanwhile, Librarian 2 can help someone looking for "The Great Gatsby" simultaneously!

**Benefits:**
1. **All librarians work in parallel** (no waiting)
2. **No keys/locks needed** (each owns their section)
3. **Each librarian memorizes their section** (super fast retrieval)

**This is the core idea.** Now let's see why computers work the same way.

---

## Part 2: CPU Memory Hierarchy (The Foundation)

Here's what most software developers don't learn in school: **Your CPU has multiple layers of memory, and they're VASTLY different in speed.**

### The Memory Hierarchy

```
┌─────────────────────────────────────────────────────────┐
│  CPU Core                                               │
│  ┌───────────────────────────────────────────────────┐ │
│  │ Registers (fastest, tiny: ~100 bytes)             │ │
│  └───────────────────────────────────────────────────┘ │
│                        ↓                                │
│  ┌───────────────────────────────────────────────────┐ │
│  │ L1 Cache (32-64 KB per core)                      │ │
│  │ Latency: 1-3 cycles (~2 nanoseconds)             │ │← YOU WANT DATA HERE
│  └───────────────────────────────────────────────────┘ │
│                        ↓                                │
│  ┌───────────────────────────────────────────────────┐ │
│  │ L2 Cache (256-512 KB per core)                    │ │
│  │ Latency: 3-15 cycles (~5 nanoseconds)            │ │
│  └───────────────────────────────────────────────────┘ │
└──────────────────────────┬──────────────────────────────┘
                           ↓
┌─────────────────────────────────────────────────────────┐
│ L3 Cache (8-32 MB, shared across all cores)            │
│ Latency: 20-40 cycles (~17 nanoseconds)                │
└──────────────────────────┬──────────────────────────────┘
                           ↓
┌─────────────────────────────────────────────────────────┐
│ RAM (8-128 GB)                                          │
│ Latency: 100-200 cycles (~60-100 nanoseconds)         │← YOU DON'T WANT DATA HERE
└─────────────────────────────────────────────────────────┘
```

Source: [Applied C++: Memory Latency Benchmarks](https://medium.com/applied/applied-c-memory-latency-d05a42fe354e)

### The Key Numbers

**For a 2.5 GHz CPU:**
- **L1 cache hit**: ~2 nanoseconds (reading a word in front of you)
- **RAM access**: ~100 nanoseconds (walking to another building to get that word)

**That's a 50x difference!**

### Why This Matters for Order Books

An order book for Apple stock might contain:
- 1000 price levels
- 5000 orders
- ~500 KB of data

**If this fits in L2 cache (512 KB):** Every access is 5 nanoseconds.
**If this must come from RAM:** Every access is 100 nanoseconds.

Processing 1 million orders:
- **Cache**: 1M × 5ns = 5 milliseconds
- **RAM**: 1M × 100ns = 100 milliseconds

**20x slower** just from memory access!

---

## Part 3: The "Bouncing Between Cores" Problem Explained

This is the critical part. Let me break it down step-by-step.

### How Cache Works (The Basics)

Each CPU core has its **own private L1 and L2 cache**:

```
Core 1              Core 2              Core 3
┌──────────┐       ┌──────────┐       ┌──────────┐
│ L1 Cache │       │ L1 Cache │       │ L1 Cache │
│ L2 Cache │       │ L2 Cache │       │ L2 Cache │
└──────────┘       └──────────┘       └──────────┘
     │                  │                  │
     └──────────────────┴──────────────────┘
                        │
                  ┌──────────┐
                  │ L3 Cache │ (Shared)
                  └──────────┘
                        │
                    ┌──────┐
                    │ RAM  │
                    └──────┘
```

When Core 1 reads the order book:
1. Data is fetched from RAM (slow: 100ns)
2. Data is stored in Core 1's L1/L2 cache
3. Next access from Core 1: **2ns** (from L1 cache!)

**Problem:** What if Core 2 needs the same order book?

### Cache Coherency (The Hidden Cost)

CPUs have a protocol called **MESI** (Modified, Exclusive, Shared, Invalid) that keeps caches synchronized.

**Rule:** If Core 1 modifies data in its cache, all other cores' copies of that data must be marked **Invalid**.

Let me show you what happens with multiple threads:

### Scenario: Multi-Threaded Order Matching

**Setup:**
- Thread A runs on Core 1
- Thread B runs on Core 2
- Thread C runs on Core 3
- All threads share the Apple order book (protected by a lock)

**Timeline:**

```
Time 0: Thread A (Core 1) acquires lock
    ↓
    Core 1 fetches Apple order book from RAM into its L1 cache (100ns)
    ↓
    Thread A processes order, modifies order book (2ns - L1 cache access)
    ↓
    Thread A releases lock

Time 100ns: Thread B (Core 2) acquires lock
    ↓
    Core 2 needs Apple order book...
    ↓
    BUT Core 1's cache has the modified version!
    ↓
    MESI protocol kicks in:
      - Core 1's cache line marked as "Shared"
      - Core 2 fetches from Core 1's cache OR from RAM
      - Takes ~50-100ns (cache coherency overhead)
    ↓
    Thread B processes order, modifies order book
    ↓
    Core 1's cache line now marked "Invalid" (erased!)
    ↓
    Thread B releases lock

Time 200ns: Thread C (Core 3) acquires lock
    ↓
    Core 3 needs Apple order book...
    ↓
    Core 2's cache has it, but Core 1 and 3 don't
    ↓
    Fetch again: ~50-100ns
    ↓
    Core 2's cache line marked "Invalid"
    ↓
    Process continues...

Time 300ns: Thread A gets lock AGAIN
    ↓
    Wait... Core 1's cache was invalidated earlier!
    ↓
    Must fetch AGAIN: ~50-100ns
    ↓
    Even though Core 1 had this data before!
```

**This is the "bouncing" problem.**

The order book physically moves between CPU cores' caches, getting invalidated and re-fetched over and over.

Source: [Cache Coherence: MESI Protocol](https://medium.com/codetodeploy/cache-coherence-how-the-mesi-protocol-keeps-multi-core-cpus-consistent-a572fbdff5d2)

### Why "50x Slower"

**Best case (data in L1 cache):** 2 nanoseconds

**Cache coherency case (data in another core's cache):** 50 nanoseconds

**Worst case (data must come from RAM):** 100 nanoseconds

That's where the **50x slower** comes from. You pay this penalty **on every lock handoff**.

### The Real Kicker: Cache Lines

CPUs don't move individual bytes—they move **cache lines** (64 bytes at a time).

Even if Thread A only modifies 8 bytes of the order book, the **entire 64-byte cache line** gets invalidated on other cores.

**This is called "false sharing"** when unrelated data on the same cache line causes unnecessary invalidations.

> "If two variables are in the same cache line and written to by different threads, they present the same problems of write contention as if they were a single variable."

Source: [LMAX Disruptor Documentation](https://lmax-exchange.github.io/disruptor/disruptor.html)

---

## Part 4: The Lock Overhead Problem

"Okay, but locks are fast, right? It's just setting a flag."

**Not quite.**

### Two Types of Lock Performance

**Uncontended (fast path):**
```c
// Thread tries to acquire lock, nobody else wants it
if (atomic_compare_and_swap(&lock, 0, 1)) {
    // Got it! Cost: ~25-50 nanoseconds
}
```

**Contended (slow path):**
```c
// Thread tries to acquire lock, but it's held by another thread
if (atomic_compare_and_swap(&lock, 0, 1)) {
    // Failed!
} else {
    // Ask kernel to put this thread to sleep
    futex_wait(&lock);  // ← EXPENSIVE: 3,000-5,000 nanoseconds!
}
```

Source: [Spinlocks vs. Mutexes Performance](https://howtech.substack.com/p/spinlocks-vs-mutexes-when-to-spin)

### What Happens on Contention

When Thread B can't get the lock:

1. **Syscall to kernel** (~500 nanoseconds)
2. **Context switch** - OS saves Thread B's state, loads another thread (~3,000-5,000 nanoseconds)
3. **Cache pollution** - Thread B's cache is now filled with other thread's data
4. When lock is released, Thread B must be woken up (another syscall + context switch)

**Total cost: 3,000-5,000 nanoseconds** instead of 25-50 nanoseconds.

Source: [Context Switch Cost Benchmarks](https://blog.tsunanet.net/2010/11/how-long-does-it-take-to-make-context.html)

**That's 100-200x slower than an uncontended lock!**

### Real-World Impact

In a busy exchange processing Apple stock:
- Orders arrive every 100-500 nanoseconds
- If lock contention causes a 5,000ns delay, you've blocked **10-50 orders**
- Latency becomes unpredictable (sometimes 100ns, sometimes 5,000ns)

This ruins **tail latency** (p99, p999). Some orders take milliseconds while most take microseconds.

---

## Part 5: Single-Threaded Solution (How It Wins)

Now let's see how single-threading eliminates all these problems.

### The Architecture

Instead of threads sharing order books:

```
BEFORE (Multi-threaded):
┌─────────────────────────────────────────────┐
│ Thread Pool (8 threads)                     │
│   ↓     ↓     ↓     ↓                       │
│ Shared Order Books (with locks)             │
│ [Apple] [Tesla] [Microsoft]                 │
└─────────────────────────────────────────────┘
     ↑ All threads compete for locks


AFTER (Single-threaded):
Core 1: [Thread 1] → Owns [Apple Order Book]     ← Pinned to Core 1
Core 2: [Thread 2] → Owns [Tesla Order Book]     ← Pinned to Core 2
Core 3: [Thread 3] → Owns [Microsoft Order Book] ← Pinned to Core 3
Core 4: [Thread 4] → Owns [Google Order Book]    ← Pinned to Core 4
...
```

**Each thread:**
- Owns ONE order book
- Runs on ONE dedicated CPU core (pinned)
- Never shares data with other threads
- **No locks needed**

### What Happens to the Order Book

**Thread 1 (Apple stock) on Core 1:**

```
Time 0: First Apple order arrives
    ↓
    Fetch order book from RAM into Core 1's L1 cache (100ns - one-time cost)
    ↓
    Process order (2ns)

Time 100ns: Second Apple order arrives
    ↓
    Order book STILL in Core 1's L1 cache!
    ↓
    Process order (2ns)

Time 200ns: Third Apple order arrives
    ↓
    STILL in cache!
    ↓
    Process order (2ns)

... 1 million orders later ...

Time 2ms: Millionth Apple order
    ↓
    STILL IN CACHE! (Core 1 is dedicated to Apple, never switches)
    ↓
    Process order (2ns)
```

**The order book never leaves Core 1's cache.**

### The Performance Math

**Processing 1 million orders:**

**Multi-threaded:**
- Lock acquire (uncontended): 50ns × 1M = 50ms
- Lock acquire (contended, 10% of time): 5,000ns × 100K = 500ms
- Cache misses (50% hit rate): 100ns × 500K = 50ms
- **Total: ~600ms**

**Single-threaded:**
- No locks: 0ms
- Cache hits (99% hit rate): 2ns × 990K = 2ms
- Cache misses (1% for new data): 100ns × 10K = 1ms
- **Total: ~3ms**

**200x faster!**

(These are simplified calculations, but the ratios match real benchmarks.)

---

## Part 6: Real-World Evidence

This isn't theory. Companies bet billions on this architecture.

### LMAX Disruptor

LMAX Exchange in London had performance problems with traditional threading. They switched to single-threaded per instrument.

**Results:**
- **6 million orders/second** per thread
- **52 nanoseconds** mean latency (vs 32,757ns with traditional queues)
- **3 orders of magnitude** lower latency
- **8x higher throughput**

They open-sourced their queue design: [LMAX Disruptor](https://lmax-exchange.github.io/disruptor/)

Martin Fowler (famous software architect) wrote about it: [The LMAX Architecture](https://martinfowler.com/articles/lmax.html)

### Modern Cryptocurrency Exchanges

Every major crypto exchange uses this pattern:

> "HFT firms operating in digital asset markets strive for low double-digit microsecond tick-to-trade performance to remain competitive."

Source: [AWS Trading Platform Optimization](https://aws.amazon.com/blogs/web3/optimize-tick-to-trade-latency-for-digital-assets-exchanges-and-trading-platforms-on-aws/)

**How they achieve it:**
- One thread per trading pair (BTC/USD, ETH/USD, etc.)
- Thread pinned to dedicated CPU core
- Lock-free queues for order submission
- Target: **1.5 microseconds** per order processing

Source: [Low Latency Trading Systems Guide](https://www.tuvoc.com/blog/low-latency-trading-systems-guide/)

### Open Source Implementation

The **exchange-core** project implements this architecture in Java:

**Performance on 10-year-old hardware (Intel Xeon X5690):**
- **5 million operations/second** per order book
- Sub-microsecond latency
- Uses LMAX Disruptor pattern

Source: [exchange-core on GitHub](https://github.com/exchange-core/exchange-core)

---

## Part 7: Common Objections Answered

### "Isn't this wasteful? You're only using 1 core per stock!"

**No.** You use ALL your cores, just differently:

**8-core server example:**
```
Core 0: Gateway thread (receives orders from network)
Core 1: AAPL order matching
Core 2: TSLA order matching
Core 3: MSFT order matching
Core 4: GOOGL order matching
Core 5: Market data publisher (sends trade updates)
Core 6: Risk management (position tracking)
Core 7: Database writer (persistence)
```

All 8 cores are busy. They just don't share data or fight over locks.

**Result:** Linear scaling. 32 cores = 20+ stocks + support services.

### "What if one stock is way busier than others?"

**That's okay!**

If Apple gets 100,000 orders/sec and Microsoft gets 1,000 orders/sec:
- Apple's thread (Core 1) runs at 100% CPU
- Microsoft's thread (Core 2) runs at 1% CPU

But Microsoft orders still get processed in microseconds (no waiting for locks).

**Alternative:** If Apple is consistently busier, you can:
- Shard Apple across 2 cores (odd order IDs → Core 1, even → Core 2)
- Give Microsoft's core to another busy stock

### "What about fairness? Can't one stock starve another?"

No starvation is possible because **each stock has a dedicated thread**.

In multi-threaded systems, if 10 threads are stuck waiting for Apple's lock, Microsoft orders wait too.

In single-threaded systems, Microsoft's thread keeps processing regardless of Apple's load.

### "Don't you need locks for the input queue?"

**Not really.** You use **lock-free queues** (single-producer, single-consumer).

**Pattern:**
```
Gateway Thread (producer) → Lock-free Ring Buffer → Matching Thread (consumer)
```

**Why it's lock-free:**
- Only 1 thread writes (gateway)
- Only 1 thread reads (matching)
- No contention possible!
- Uses atomic operations for index updates (~25ns, no context switch)

The LMAX Disruptor is specifically designed for this pattern.

---

## Part 8: When Multi-Threading IS Better

Single-threading isn't always the answer. Here's when to use each:

### Use Multi-Threading (Thread Pools) When:

✅ **Tasks are independent**
- Processing 1000 customer emails (no shared state)
- Rendering video frames (each frame separate)
- Web server handling HTTP requests (separate requests)

✅ **Work involves waiting (I/O)**
- Database queries (thread blocks waiting for DB)
- Network requests (thread blocks waiting for response)
- File operations (thread blocks on disk I/O)

✅ **CPU-bound with no shared state**
- Image processing (each image independent)
- Data analytics (map-reduce style)
- Scientific computing (parallel algorithms)

✅ **High variance in task duration**
- Some requests take 1ms, others take 1 second
- Thread pools handle this gracefully

### Use Single-Threading (Dedicated Threads) When:

✅ **Low latency is critical**
- Sub-millisecond response times required
- Tail latency (p99, p999) matters
- Predictable performance needed

✅ **Working set fits in cache**
- Order book: ~500KB (fits in L2 cache)
- Game state: ~1MB (fits in L3 cache)
- In-memory database: working set < cache size

✅ **Hot path, tight loop**
- Matching millions of orders/second
- Real-time game server (tick loop)
- High-frequency data processing

✅ **Shared mutable state**
- If you'd need locks with multi-threading, consider single-threading instead
- One thread owns the state = no locks

---

## Part 9: The Mental Model Shift

### Traditional CS Education

**What you learn:**
> "Parallelism = performance. Use all your cores. N cores = N× faster."

**Mental model:**
```
More threads = More speed
```

### Systems Performance Reality

**What actually matters:**
> "Memory access patterns determine performance. Cache hits vs cache misses matter more than core count."

**Better mental model:**
```
Fast threads = Threads that don't wait

What makes threads slow?
1. Waiting for locks (contention)
2. Waiting for data (cache misses)
3. Context switches (kernel overhead)

Eliminating wait time > Adding more threads
```

### Mechanical Sympathy

Martin Thompson (LMAX architect) coined the term **"Mechanical Sympathy"** for this approach:

> "You don't have to be an engineer to be a racing driver, but you do have to have Mechanical Sympathy."
> — Jackie Stewart, Formula 1 champion

**Applied to software:**

Understanding how CPUs actually work (caches, coherency, context switches) makes you write faster code.

**Not:**
```java
// "I'll just add more threads, CPUs are fast!"
ExecutorService pool = Executors.newFixedThreadPool(32);
```

**Instead:**
```java
// "I'll keep hot data in L1 cache and avoid locks."
Thread matchingThread = new Thread(() -> {
    // Pin to CPU core
    // Process orders in tight loop
    // No locks, data stays in cache
});
```

Source: [Martin Thompson: Mechanical Sympathy (SE Radio)](https://se-radio.net/2014/02/episode-201-martin-thompson-on-mechanical-sympathy/)

---

## Part 10: Practical Takeaways

### For Matching Engines / Trading Systems

**Do this:**
- 1 thread per instrument (stock/crypto pair)
- Pin threads to CPU cores
- Use lock-free SPSC (single-producer single-consumer) queues
- Preallocate order book memory (avoid GC/allocation in hot path)
- Keep working set < L2 cache size (512KB)

**Avoid:**
- Thread pools for matching logic
- Locks in the hot path
- Sharing order books between threads
- Dynamic allocation during matching

### For Other Low-Latency Systems

**This pattern works for:**
- Real-time game servers (tick loop per game world)
- In-memory databases (query thread per partition)
- Event processing (stream processing per partition)
- Network packet processing (thread per queue)

**Key requirement:** Working set must fit in cache.

### How to Measure

**Metrics that matter:**
- **p50, p99, p999 latency** (not just average)
- **Cache hit rate** (use `perf stat -e cache-misses`)
- **Context switches** (use `perf stat -e context-switches`)
- **Lock contention** (profiling tools show this)

**Red flags:**
- High p99 latency (lock contention)
- Many context switches (>1000/sec per thread)
- Low cache hit rate (<90%)

---

## Summary: The Complete Picture

### Why Multi-Threading Seems Right But Isn't

**Intuition:** 8 cores × 8 threads = 8× speed

**Reality:**
- Threads fight for locks (serialization)
- Data bounces between cores (cache coherency overhead)
- Context switches waste time (kernel overhead)
- Result: **Slower than single-threaded**

### Why Single-Threading Wins

**The magic:**
1. **Cache locality** - Data stays in L1/L2 cache (2-5ns access instead of 100ns RAM)
2. **No locks** - Eliminates 50-5,000ns overhead per operation
3. **No context switches** - Saves 3,000-5,000ns per switch
4. **No cache coherency** - No MESI protocol overhead
5. **Predictable latency** - No contention spikes

**The math:**
- Multi-threaded: ~500-1,000ns per operation (locks + cache misses + context switches)
- Single-threaded: ~50-100ns per operation (L1 cache + no locks)
- **10-20× faster**

### The Real-World Proof

- **LMAX:** 6M orders/sec, 52ns latency
- **Modern exchanges:** 1.5μs per order
- **Open source (exchange-core):** 5M ops/sec on old hardware

**All use the same pattern:** One thread, one core, no locks, cache-hot data.

### The Insight

> **The bottleneck isn't CPU speed—it's memory access patterns.**

Modern CPUs are incredibly fast at compute (billions of ops/sec) but relatively slow at memory access (100ns per RAM fetch).

**Better to:**
- Do less work but keep data in cache (single-threaded)

**Than to:**
- Do more work in parallel but keep missing cache (multi-threaded)

---

## Further Reading

**Essential:**
- [The LMAX Architecture](https://martinfowler.com/articles/lmax.html) - The original paper
- [LMAX Disruptor](https://lmax-exchange.github.io/disruptor/) - Production-grade implementation
- [Mechanical Sympathy Blog](https://mechanical-sympathy.blogspot.com/) - Martin Thompson's deep dives

**Technical Deep Dives:**
- [Applied C++: Memory Latency Benchmarks](https://medium.com/applied/applied-c-memory-latency-d05a42fe354e)
- [Cache Coherence: MESI Protocol](https://medium.com/codetodeploy/cache-coherence-how-the-mesi-protocol-keeps-multi-core-cpus-consistent-a572fbdff5d2)
- [Context Switch Performance](https://blog.tsunanet.net/2010/11/how-long-does-it-take-to-make-context.html)
- [Basics of Futexes](https://eli.thegreenplace.net/2018/basics-of-futexes/)

**Open Source:**
- [exchange-core](https://github.com/exchange-core/exchange-core) - Production matching engine
- [LMAX Disruptor on GitHub](https://github.com/LMAX-Exchange/disruptor)

**Performance Analysis:**
- [Low Latency Trading Systems](https://www.tuvoc.com/blog/low-latency-trading-systems-guide/)
- [AWS Trading Platform Optimization](https://aws.amazon.com/blogs/web3/optimize-tick-to-trade-latency-for-digital-assets-exchanges-and-trading-platforms-on-aws/)

---

## Conclusion

The next time someone says "just add more threads to make it faster," remember:

**For cache-sensitive, latency-critical workloads:**
- Memory access speed > CPU core count
- Cache hits > Parallel execution
- No locks > Many threads

Understanding your CPU's memory hierarchy and cache coherency protocol is the difference between:
- **750,000 orders/second** (multi-threaded with locks)
- **6,000,000 orders/second** (single-threaded with cache locality)

That's mechanical sympathy in action.
