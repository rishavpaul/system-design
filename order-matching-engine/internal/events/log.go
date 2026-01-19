package events

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// EventLog is an append-only, durable event log.
//
// Design Decisions:
//
// 1. Binary Format: We use gob encoding for simplicity, but production systems
//    would use a more compact format (protobuf, flatbuffers, or custom binary).
//
// 2. Checksums: Each event has a CRC32 checksum to detect corruption.
//
// 3. Sync Options: We support both synchronous (fsync per write) and asynchronous
//    modes. Sync mode guarantees durability but is slower.
//
// 4. Sequence Numbers: Each event has a monotonically increasing sequence number
//    for gap detection and ordering.
//
// Production Considerations:
// - Real systems use write-ahead logs (WAL) with battery-backed RAM
// - Segment files (rotate when size limit reached) for easy cleanup
// - Compression for storage efficiency
// - Replication for fault tolerance
type EventLog struct {
	file        *os.File
	writer      *bufio.Writer
	encoder     *gob.Encoder
	mu          sync.Mutex
	sequenceNum uint64
	syncMode    bool // If true, fsync after every write
	path        string
}

// EventLogConfig configures the event log.
type EventLogConfig struct {
	Path     string
	SyncMode bool // If true, fsync after every write (slower but durable)
}

// NewEventLog creates a new event log.
func NewEventLog(config EventLogConfig) (*EventLog, error) {
	file, err := os.OpenFile(config.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open event log: %w", err)
	}

	writer := bufio.NewWriter(file)

	log := &EventLog{
		file:     file,
		writer:   writer,
		encoder:  gob.NewEncoder(writer),
		syncMode: config.SyncMode,
		path:     config.Path,
	}

	// Read existing events to get last sequence number
	if err := log.recover(); err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to recover event log: %w", err)
	}

	return log, nil
}

// eventRecord is the on-disk format for events.
type eventRecord struct {
	SequenceNum uint64
	Type        EventType
	Data        interface{}
	Checksum    uint32
}

// Append writes an event to the log.
// Returns the sequence number assigned to the event.
func (l *EventLog) Append(event interface{}) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sequenceNum++
	seqNum := l.sequenceNum

	// Set sequence number on the event
	switch e := event.(type) {
	case *NewOrderEvent:
		e.SequenceNum = seqNum
	case *CancelOrderEvent:
		e.SequenceNum = seqNum
	case *OrderAcceptedEvent:
		e.SequenceNum = seqNum
	case *OrderRejectedEvent:
		e.SequenceNum = seqNum
	case *FillEvent:
		e.SequenceNum = seqNum
	case *OrderCancelledEvent:
		e.SequenceNum = seqNum
	}

	// Create record
	record := eventRecord{
		SequenceNum: seqNum,
		Data:        event,
	}

	// Calculate checksum (simplified - real impl would checksum encoded bytes)
	record.Checksum = crc32.ChecksumIEEE([]byte(fmt.Sprintf("%v", event)))

	// Write length prefix (for easier recovery)
	// In production, we'd write: [length][type][data][checksum]
	if err := l.encoder.Encode(record); err != nil {
		return 0, fmt.Errorf("failed to encode event: %w", err)
	}

	// Flush buffer
	if err := l.writer.Flush(); err != nil {
		return 0, fmt.Errorf("failed to flush: %w", err)
	}

	// Sync to disk if in sync mode
	if l.syncMode {
		if err := l.file.Sync(); err != nil {
			return 0, fmt.Errorf("failed to sync: %w", err)
		}
	}

	return seqNum, nil
}

// Replay reads all events and calls the handler for each.
// Used to rebuild state after restart.
func (l *EventLog) Replay(handler func(seqNum uint64, event interface{}) error) error {
	// Open a separate file handle for reading
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Empty log
		}
		return fmt.Errorf("failed to open for replay: %w", err)
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	var lastSeq uint64

	for {
		var record eventRecord
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode event: %w", err)
		}

		// Check for gaps
		if lastSeq > 0 && record.SequenceNum != lastSeq+1 {
			return fmt.Errorf("sequence gap detected: expected %d, got %d",
				lastSeq+1, record.SequenceNum)
		}
		lastSeq = record.SequenceNum

		// Verify checksum (simplified)
		expectedChecksum := crc32.ChecksumIEEE([]byte(fmt.Sprintf("%v", record.Data)))
		if record.Checksum != expectedChecksum {
			return fmt.Errorf("checksum mismatch at sequence %d", record.SequenceNum)
		}

		if err := handler(record.SequenceNum, record.Data); err != nil {
			return fmt.Errorf("handler error at sequence %d: %w", record.SequenceNum, err)
		}
	}

	return nil
}

// recover reads the log to find the last sequence number.
func (l *EventLog) recover() error {
	file, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // New log
		}
		return err
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)

	for {
		var record eventRecord
		if err := decoder.Decode(&record); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		l.sequenceNum = record.SequenceNum
	}

	return nil
}

// GetLastSequence returns the last sequence number.
func (l *EventLog) GetLastSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sequenceNum
}

// Sync forces a flush to disk.
func (l *EventLog) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.writer.Flush(); err != nil {
		return err
	}
	return l.file.Sync()
}

// Close closes the event log.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.writer.Flush(); err != nil {
		return err
	}
	return l.file.Close()
}

// Register gob types for encoding/decoding
func init() {
	gob.Register(&NewOrderEvent{})
	gob.Register(&CancelOrderEvent{})
	gob.Register(&OrderAcceptedEvent{})
	gob.Register(&OrderRejectedEvent{})
	gob.Register(&FillEvent{})
	gob.Register(&OrderCancelledEvent{})
}
