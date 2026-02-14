package main

import "time"

// ServerState represents the state of a Raft server
type ServerState int

const (
	Follower ServerState = iota
	Candidate
	Leader
)

func (s ServerState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// LogEntry represents a single entry in the replicated log
type LogEntry struct {
	Term    int
	Index   int
	Command interface{}
}

// RequestVoteArgs is the RPC request for voting
type RequestVoteArgs struct {
	Term         int
	CandidateID  int
	LastLogIndex int
	LastLogTerm  int
}

// RequestVoteReply is the RPC response for voting
type RequestVoteReply struct {
	Term        int
	VoteGranted bool
}

// AppendEntriesArgs is the RPC request for log replication (and heartbeat)
type AppendEntriesArgs struct {
	Term         int
	LeaderID     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

// AppendEntriesReply is the RPC response for log replication
type AppendEntriesReply struct {
	Term    int
	Success bool
}

// ApplyMsg represents a message to apply to the state machine
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
}

// Config for timing (in milliseconds)
const (
	HeartbeatInterval = 100 * time.Millisecond
	ElectionTimeoutMin = 300 * time.Millisecond
	ElectionTimeoutMax = 600 * time.Millisecond
)
