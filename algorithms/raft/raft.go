package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// Raft represents a single Raft node
type Raft struct {
	mu        sync.Mutex
	id        int
	peers     []*Raft
	dead      bool
	applyCh   chan ApplyMsg

	// Persistent state
	currentTerm int
	votedFor    int
	log         []LogEntry

	// Volatile state
	state       ServerState
	commitIndex int
	lastApplied int

	// Leader state (reinitialized after election)
	nextIndex  []int
	matchIndex []int

	// Timing
	electionTimeout  time.Duration
	lastHeartbeat    time.Time
	heartbeatTicker  *time.Ticker
	electionTimer    *time.Timer
}

// NewRaft creates a new Raft instance
func NewRaft(id int, peers []*Raft, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{
		id:           id,
		peers:        peers,
		applyCh:      applyCh,
		currentTerm:  0,
		votedFor:     -1,
		log:          []LogEntry{{Term: 0, Index: 0}}, // Dummy entry at index 0
		state:        Follower,
		commitIndex:  0,
		lastApplied:  0,
		lastHeartbeat: time.Now(),
	}

	rf.resetElectionTimeout()

	// Start background goroutines
	go rf.electionDaemon()
	go rf.heartbeatDaemon()
	go rf.applyDaemon()

	return rf
}

// Kill marks the Raft node as dead
func (rf *Raft) Kill() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.dead = true
	if rf.electionTimer != nil {
		rf.electionTimer.Stop()
	}
	if rf.heartbeatTicker != nil {
		rf.heartbeatTicker.Stop()
	}
}

// GetState returns the current term and whether this server is the leader
func (rf *Raft) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.currentTerm, rf.state == Leader
}

// Start starts agreement on a new log entry
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.state != Leader {
		return -1, rf.currentTerm, false
	}

	index := len(rf.log)
	term := rf.currentTerm
	entry := LogEntry{
		Term:    term,
		Index:   index,
		Command: command,
	}
	rf.log = append(rf.log, entry)

	fmt.Printf("[Node %d] Leader accepted command: %v at index %d\n", rf.id, command, index)

	// Trigger immediate replication
	go rf.replicateToAll()

	return index, term, true
}

// resetElectionTimeout resets the election timeout to a random value
func (rf *Raft) resetElectionTimeout() {
	min := int(ElectionTimeoutMin.Milliseconds())
	max := int(ElectionTimeoutMax.Milliseconds())
	timeout := time.Duration(min + rand.Intn(max-min)) * time.Millisecond
	rf.electionTimeout = timeout
	rf.lastHeartbeat = time.Now()
}

// electionDaemon monitors election timeout and starts elections
func (rf *Raft) electionDaemon() {
	for {
		time.Sleep(50 * time.Millisecond)

		rf.mu.Lock()
		if rf.dead {
			rf.mu.Unlock()
			return
		}

		// Only followers and candidates can start elections
		if rf.state != Leader && time.Since(rf.lastHeartbeat) > rf.electionTimeout {
			rf.mu.Unlock()
			rf.startElection()
		} else {
			rf.mu.Unlock()
		}
	}
}

// startElection initiates a leader election
func (rf *Raft) startElection() {
	rf.mu.Lock()
	rf.state = Candidate
	rf.currentTerm++
	rf.votedFor = rf.id
	rf.resetElectionTimeout()

	currentTerm := rf.currentTerm
	candidateID := rf.id
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term

	fmt.Printf("[Node %d] Starting election for term %d\n", rf.id, currentTerm)
	rf.mu.Unlock()

	votes := 1
	var voteMu sync.Mutex

	// Request votes from all peers
	for i, peer := range rf.peers {
		if i == rf.id {
			continue
		}

		go func(peer *Raft, serverID int) {
			args := RequestVoteArgs{
				Term:         currentTerm,
				CandidateID:  candidateID,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  lastLogTerm,
			}
			reply := RequestVoteReply{}

			ok := peer.RequestVote(&args, &reply)
			if !ok {
				return
			}

			rf.mu.Lock()
			defer rf.mu.Unlock()

			// Check if term has changed
			if rf.currentTerm != currentTerm || rf.state != Candidate {
				return
			}

			// Update term if we're behind
			if reply.Term > rf.currentTerm {
				rf.currentTerm = reply.Term
				rf.state = Follower
				rf.votedFor = -1
				return
			}

			// Count vote
			if reply.VoteGranted {
				voteMu.Lock()
				votes++
				currentVotes := votes
				voteMu.Unlock()

				// Check if we won the election
				majority := len(rf.peers)/2 + 1
				if currentVotes >= majority && rf.state == Candidate {
					rf.becomeLeader()
				}
			}
		}(peer, i)
	}
}

// becomeLeader transitions the node to leader state
func (rf *Raft) becomeLeader() {
	rf.state = Leader
	fmt.Printf("[Node %d] Became LEADER for term %d\n", rf.id, rf.currentTerm)

	// Initialize leader state
	rf.nextIndex = make([]int, len(rf.peers))
	rf.matchIndex = make([]int, len(rf.peers))
	for i := range rf.peers {
		rf.nextIndex[i] = len(rf.log)
		rf.matchIndex[i] = 0
	}

	// Send immediate heartbeat
	go rf.replicateToAll()
}

// heartbeatDaemon sends periodic heartbeats when leader
func (rf *Raft) heartbeatDaemon() {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	for {
		<-ticker.C

		rf.mu.Lock()
		if rf.dead {
			rf.mu.Unlock()
			return
		}

		if rf.state == Leader {
			rf.mu.Unlock()
			rf.replicateToAll()
		} else {
			rf.mu.Unlock()
		}
	}
}

// replicateToAll sends AppendEntries to all peers
func (rf *Raft) replicateToAll() {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.state != Leader {
		return
	}

	for i := range rf.peers {
		if i == rf.id {
			continue
		}

		go rf.replicateToPeer(i)
	}
}

// replicateToPeer sends AppendEntries to a specific peer
func (rf *Raft) replicateToPeer(serverID int) {
	rf.mu.Lock()
	if rf.state != Leader || rf.dead {
		rf.mu.Unlock()
		return
	}

	nextIdx := rf.nextIndex[serverID]
	prevLogIndex := nextIdx - 1
	prevLogTerm := rf.log[prevLogIndex].Term

	entries := []LogEntry{}
	if nextIdx < len(rf.log) {
		entries = append(entries, rf.log[nextIdx:]...)
	}

	args := AppendEntriesArgs{
		Term:         rf.currentTerm,
		LeaderID:     rf.id,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: rf.commitIndex,
	}
	rf.mu.Unlock()

	reply := AppendEntriesReply{}
	ok := rf.peers[serverID].AppendEntries(&args, &reply)
	if !ok {
		return
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	// Check if we're still leader and term hasn't changed
	if rf.state != Leader || rf.currentTerm != args.Term {
		return
	}

	// Update term if we're behind
	if reply.Term > rf.currentTerm {
		rf.currentTerm = reply.Term
		rf.state = Follower
		rf.votedFor = -1
		return
	}

	if reply.Success {
		// Update match and next indices
		rf.matchIndex[serverID] = prevLogIndex + len(entries)
		rf.nextIndex[serverID] = rf.matchIndex[serverID] + 1

		// Check if we can commit more entries
		rf.updateCommitIndex()
	} else {
		// Decrement nextIndex and retry
		rf.nextIndex[serverID] = max(1, rf.nextIndex[serverID]-1)
	}
}

// updateCommitIndex advances commitIndex based on matchIndex
func (rf *Raft) updateCommitIndex() {
	for n := rf.commitIndex + 1; n < len(rf.log); n++ {
		if rf.log[n].Term != rf.currentTerm {
			continue
		}

		count := 1 // Count self
		for i := range rf.peers {
			if i != rf.id && rf.matchIndex[i] >= n {
				count++
			}
		}

		if count > len(rf.peers)/2 {
			rf.commitIndex = n
			fmt.Printf("[Node %d] Committed entry at index %d: %v\n", rf.id, n, rf.log[n].Command)
		}
	}
}

// applyDaemon applies committed entries to the state machine
func (rf *Raft) applyDaemon() {
	for {
		time.Sleep(10 * time.Millisecond)

		rf.mu.Lock()
		if rf.dead {
			rf.mu.Unlock()
			return
		}

		// Apply committed entries
		for rf.lastApplied < rf.commitIndex {
			rf.lastApplied++
			entry := rf.log[rf.lastApplied]

			msg := ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandIndex: entry.Index,
			}

			rf.mu.Unlock()
			rf.applyCh <- msg
			rf.mu.Lock()
		}
		rf.mu.Unlock()
	}
}

// RequestVote handles RequestVote RPC
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.dead {
		return false
	}

	// Update term if we're behind
	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
	}

	reply.Term = rf.currentTerm
	reply.VoteGranted = false

	// Reject if candidate's term is old
	if args.Term < rf.currentTerm {
		return true
	}

	// Check if we've already voted
	if rf.votedFor != -1 && rf.votedFor != args.CandidateID {
		return true
	}

	// Check if candidate's log is at least as up-to-date
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term

	logUpToDate := args.LastLogTerm > lastLogTerm ||
		(args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex)

	if logUpToDate {
		rf.votedFor = args.CandidateID
		rf.resetElectionTimeout()
		reply.VoteGranted = true
		fmt.Printf("[Node %d] Voted for Node %d in term %d\n", rf.id, args.CandidateID, args.Term)
	}

	return true
}

// AppendEntries handles AppendEntries RPC (heartbeat and log replication)
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.dead {
		return false
	}

	// Update term if we're behind
	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
	}

	reply.Term = rf.currentTerm
	reply.Success = false

	// Reject if leader's term is old
	if args.Term < rf.currentTerm {
		return true
	}

	// Reset election timeout (we heard from leader)
	rf.resetElectionTimeout()
	rf.state = Follower

	// Check if log contains entry at prevLogIndex with matching term
	if args.PrevLogIndex >= len(rf.log) || rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		return true
	}

	// Append new entries
	for i, entry := range args.Entries {
		index := args.PrevLogIndex + 1 + i
		if index < len(rf.log) {
			// Conflict: delete existing entry and all that follow
			if rf.log[index].Term != entry.Term {
				rf.log = rf.log[:index]
				rf.log = append(rf.log, entry)
			}
		} else {
			rf.log = append(rf.log, entry)
		}
	}

	// Update commit index
	if args.LeaderCommit > rf.commitIndex {
		rf.commitIndex = min(args.LeaderCommit, len(rf.log)-1)
	}

	reply.Success = true
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
