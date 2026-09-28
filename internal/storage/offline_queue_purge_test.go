package storage

import (
	"path/filepath"
	"testing"
)

// The "clear queue" button has to remove the backlog it claims to clear, and
// must count the removed rows so the caller can report a real number.
func TestOfflineQueuePurgeRemovesBacklog(t *testing.T) {
	q, err := NewOfflineQueue(filepath.Join(t.TempDir(), "offline_purge.db"))
	if err != nil {
		t.Fatalf("NewOfflineQueue failed: %v", err)
	}
	defer q.Close()

	for i := 0; i < 3; i++ {
		if err := q.Enqueue(map[string]interface{}{"i": i}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	// Leave one row claimed: a purge during a flush must not leave it behind.
	claimed, err := q.Dequeue()
	if err != nil || claimed == nil {
		t.Fatalf("Dequeue: %v (%v)", err, claimed)
	}

	n, err := q.Purge()
	if err != nil {
		t.Fatalf("Purge failed: %v", err)
	}
	if n != 3 {
		t.Fatalf("Purge removed %d rows, want 3", n)
	}
	if pending, _, _ := q.Stats(); pending != 0 {
		t.Fatalf("queue must be empty after purge, pending=%d", pending)
	}
	if dropped := q.Dropped(); dropped < 3 {
		t.Fatalf("purged rows must be reported as dropped, got %d", dropped)
	}
	// Acking a row the purge already deleted must stay a no-op, not an error.
	if err := q.Ack(claimed.ID); err != nil {
		t.Fatalf("Ack of a purged row must not fail: %v", err)
	}

	if n, err := q.Purge(); err != nil || n != 0 {
		t.Fatalf("purging an empty queue must report 0 removed, got %d (%v)", n, err)
	}
}
