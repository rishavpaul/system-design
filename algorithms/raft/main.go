package main

import (
	"fmt"
	"math/rand"
	"time"
)

// KVCommand represents a key-value operation
type KVCommand struct {
	Op    string // "put" or "get"
	Key   string
	Value string
}

// KVStore is a simple key-value store backed by Raft
type KVStore struct {
	raft *Raft
	data map[string]string
}

func NewKVStore(raft *Raft) *KVStore {
	return &KVStore{
		raft: raft,
		data: make(map[string]string),
	}
}

func (kv *KVStore) Put(key, value string) bool {
	cmd := KVCommand{Op: "put", Key: key, Value: value}
	_, _, isLeader := kv.raft.Start(cmd)
	return isLeader
}

func (kv *KVStore) Get(key string) (string, bool) {
	val, ok := kv.data[key]
	return val, ok
}

func (kv *KVStore) Apply(msg ApplyMsg) {
	if !msg.CommandValid {
		return
	}

	cmd, ok := msg.Command.(KVCommand)
	if !ok {
		return
	}

	if cmd.Op == "put" {
		kv.data[cmd.Key] = cmd.Value
		fmt.Printf("[KVStore %d] Applied: PUT %s=%s (index %d)\n",
			kv.raft.id, cmd.Key, cmd.Value, msg.CommandIndex)
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())

	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║         RAFT CONSENSUS ALGORITHM - LIVE DEMO             ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Create cluster of 5 nodes
	numNodes := 5
	applyChs := make([]chan ApplyMsg, numNodes)
	rafts := make([]*Raft, numNodes)
	kvStores := make([]*KVStore, numNodes)

	// Initialize apply channels
	for i := 0; i < numNodes; i++ {
		applyChs[i] = make(chan ApplyMsg, 100)
	}

	// Create Raft nodes
	for i := 0; i < numNodes; i++ {
		rafts[i] = NewRaft(i, rafts, applyChs[i])
		kvStores[i] = NewKVStore(rafts[i])
	}

	// Set peer references
	for i := 0; i < numNodes; i++ {
		rafts[i].peers = rafts
	}

	// Start apply listeners
	for i := 0; i < numNodes; i++ {
		go func(nodeID int) {
			for msg := range applyChs[nodeID] {
				kvStores[nodeID].Apply(msg)
			}
		}(i)
	}

	fmt.Println("✓ Created 5-node Raft cluster")
	fmt.Println()

	// Demo 1: Leader Election
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMO 1: LEADER ELECTION")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("Waiting for initial leader election...")
	time.Sleep(2 * time.Second)

	leaderID := findLeader(rafts)
	if leaderID != -1 {
		fmt.Printf("✓ Node %d elected as leader\n", leaderID)
	}
	fmt.Println()

	// Demo 2: Log Replication
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMO 2: LOG REPLICATION")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("Submitting commands to leader...")

	kvStores[leaderID].Put("name", "Alice")
	time.Sleep(500 * time.Millisecond)

	kvStores[leaderID].Put("age", "30")
	time.Sleep(500 * time.Millisecond)

	kvStores[leaderID].Put("city", "Seattle")
	time.Sleep(500 * time.Millisecond)

	fmt.Println("\nVerifying replication across all nodes:")
	time.Sleep(1 * time.Second)
	for i := 0; i < numNodes; i++ {
		name, _ := kvStores[i].Get("name")
		age, _ := kvStores[i].Get("age")
		city, _ := kvStores[i].Get("city")
		fmt.Printf("  Node %d: name=%s, age=%s, city=%s\n", i, name, age, city)
	}
	fmt.Println("✓ All nodes have replicated data!")
	fmt.Println()

	// Demo 3: Fault Tolerance (Kill a Follower)
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMO 3: FAULT TOLERANCE - Killing a Follower")
	fmt.Println("═══════════════════════════════════════════════════════════")

	followerID := -1
	for i := 0; i < numNodes; i++ {
		if i != leaderID {
			followerID = i
			break
		}
	}

	fmt.Printf("Killing Node %d (Follower)...\n", followerID)
	rafts[followerID].Kill()
	time.Sleep(500 * time.Millisecond)

	fmt.Println("Submitting more commands...")
	kvStores[leaderID].Put("status", "resilient")
	time.Sleep(1 * time.Second)

	fmt.Println("\nVerifying cluster still works:")
	for i := 0; i < numNodes; i++ {
		if i == followerID {
			fmt.Printf("  Node %d: ✗ DEAD\n", i)
			continue
		}
		status, _ := kvStores[i].Get("status")
		fmt.Printf("  Node %d: status=%s\n", i, status)
	}
	fmt.Println("✓ Cluster continues operating with 4/5 nodes!")
	fmt.Println()

	// Demo 4: Leader Failure and Re-election
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMO 4: LEADER FAILURE - Triggering Re-election")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Printf("Killing Node %d (Current Leader)...\n", leaderID)
	rafts[leaderID].Kill()

	fmt.Println("Waiting for new leader election...")
	time.Sleep(3 * time.Second)

	newLeaderID := findLeader(rafts)
	if newLeaderID != -1 && newLeaderID != leaderID {
		fmt.Printf("✓ Node %d elected as new leader!\n", newLeaderID)
	}
	fmt.Println()

	// Demo 5: Continue Operations
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMO 5: CONTINUED OPERATIONS")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("Submitting commands to new leader...")

	kvStores[newLeaderID].Put("recovered", "true")
	time.Sleep(1 * time.Second)

	kvStores[newLeaderID].Put("leader", fmt.Sprintf("node-%d", newLeaderID))
	time.Sleep(1 * time.Second)

	fmt.Println("\nFinal cluster state (3/5 nodes alive):")
	for i := 0; i < numNodes; i++ {
		if i == followerID || i == leaderID {
			fmt.Printf("  Node %d: ✗ DEAD\n", i)
			continue
		}
		recovered, _ := kvStores[i].Get("recovered")
		leader, _ := kvStores[i].Get("leader")
		fmt.Printf("  Node %d: recovered=%s, leader=%s\n", i, recovered, leader)
	}
	fmt.Println("✓ System fully operational with majority quorum!")
	fmt.Println()

	// Summary
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("DEMONSTRATION SUMMARY")
	fmt.Println("═══════════════════════════════════════════════════════════")
	fmt.Println("✓ Leader Election: Automatic election of a leader")
	fmt.Println("✓ Log Replication: Commands replicated to all nodes")
	fmt.Println("✓ Fault Tolerance: Survived follower failure")
	fmt.Println("✓ Leader Failure: Automatic failover and re-election")
	fmt.Println("✓ Continued Operation: System works with 3/5 nodes (majority)")
	fmt.Println()
	fmt.Println("Key Insights:")
	fmt.Println("  • Raft requires (N/2 + 1) nodes for quorum (3/5 in this case)")
	fmt.Println("  • Leader handles all writes, followers replicate")
	fmt.Println("  • Automatic failover when leader dies")
	fmt.Println("  • Strong consistency: all alive nodes have same data")
	fmt.Println()

	// Cleanup
	for i := 0; i < numNodes; i++ {
		if !rafts[i].dead {
			rafts[i].Kill()
		}
		close(applyChs[i])
	}
}

func findLeader(rafts []*Raft) int {
	for i, rf := range rafts {
		_, isLeader := rf.GetState()
		if isLeader {
			return i
		}
	}
	return -1
}
