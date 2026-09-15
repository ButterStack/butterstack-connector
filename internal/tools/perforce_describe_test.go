package tools

import (
	"strconv"
	"testing"
)

// #1904: max_bytes used to short-circuit with an error instead of truncating,
// contradicting PROTOCOL.md ("the connector truncates and sets truncated: true
// rather than streaming unbounded"). That is why changelist 118 on project 108
// - 503 files, an ordinary asset reorganisation - never synced at all rather
// than syncing partially.
//
// describe() reads a p4 -Mj record, so these drive it through that record shape
// rather than through a live p4.
func describeRecord(fileCount int) map[string]any {
	r := map[string]any{
		"change": "118",
		"user":   "ryan",
		"client": "bsg-cp-01",
		"time":   "1757260800",
		"desc":   "audio stack, asset reorg",
		"status": "submitted",
	}
	for i := 0; i < fileCount; i++ {
		r["depotFile"+strconv.Itoa(i)] = "//pilot_light/art/texture_" + strconv.Itoa(i) + ".png"
		r["action"+strconv.Itoa(i)] = "add"
		r["type"+strconv.Itoa(i)] = "binary"
		r["rev"+strconv.Itoa(i)] = "1"
	}
	return r
}

func collect(t *testing.T, fileCount, maxFiles, maxBytes int) (Describe, bool) {
	t.Helper()
	out, truncated := describeFromRecord(describeRecord(fileCount), 118, maxFiles, maxBytes)
	return out, truncated
}

func TestDescribeTruncatesOnMaxFiles(t *testing.T) {
	out, truncated := collect(t, 503, 200, DefaultDescribeMaxBytes)

	if !truncated {
		t.Fatal("a file list cut short by max_files must report truncated")
	}
	if out.FileCount != 200 {
		t.Errorf("FileCount = %d, want 200 (what was returned)", out.FileCount)
	}
	if out.DepotFileCount != 503 {
		t.Errorf("DepotFileCount = %d, want 503 (what exists) - without this a consumer cannot tell how much is missing", out.DepotFileCount)
	}
}

// The behaviour change this issue is about: a byte budget shapes the response,
// it does not fail the command.
func TestDescribeTruncatesOnMaxBytesRatherThanFailing(t *testing.T) {
	out, truncated := collect(t, 503, 100000, 4096)

	if !truncated {
		t.Fatal("a file list cut short by max_bytes must report truncated, not error")
	}
	if out.FileCount == 0 {
		t.Fatal("a tight byte budget must still return the records that fit, not nothing")
	}
	if out.FileCount >= 503 {
		t.Fatalf("FileCount = %d: the byte budget was not applied", out.FileCount)
	}
	if out.DepotFileCount != 503 {
		t.Errorf("DepotFileCount = %d, want 503", out.DepotFileCount)
	}
}

// Changelist 118's own shape. Under the old 65536 cap this was the failing
// case; it must now come back whole.
func TestDescribeReturnsAFiveHundredFileChangelistWhole(t *testing.T) {
	out, truncated := collect(t, 503, 100000, DefaultDescribeMaxBytes)

	if truncated {
		t.Error("503 files is an ordinary changelist and must not truncate at the default budget")
	}
	if out.FileCount != 503 {
		t.Errorf("FileCount = %d, want 503", out.FileCount)
	}
	if out.DepotFileCount != out.FileCount {
		t.Errorf("a complete record must have DepotFileCount == FileCount, got %d vs %d", out.DepotFileCount, out.FileCount)
	}
}

func TestDescribeCompleteListIsNotMarkedTruncated(t *testing.T) {
	out, truncated := collect(t, 12, 200, DefaultDescribeMaxBytes)

	if truncated {
		t.Error("a list under both bounds must not be marked truncated")
	}
	if out.FileCount != 12 || out.DepotFileCount != 12 {
		t.Errorf("FileCount/DepotFileCount = %d/%d, want 12/12", out.FileCount, out.DepotFileCount)
	}
}

// The budget must hold, not be discovered one record after it was crossed.
func TestDescribeStaysWithinItsByteBudget(t *testing.T) {
	const budget = 8192
	out, _ := collect(t, 503, 100000, budget)

	total := 0
	for _, f := range out.Files {
		total += f.approxJSONBytes()
	}
	if total > budget {
		t.Errorf("returned records cost %d bytes against a %d budget", total, budget)
	}
}

// A zero budget means "no caller preference", not "return nothing".
func TestDescribeZeroMaxBytesUsesTheDefault(t *testing.T) {
	out, truncated := collect(t, 503, 100000, 0)

	if truncated || out.FileCount != 503 {
		t.Errorf("max_bytes=0 should fall back to the default budget; got truncated=%v FileCount=%d", truncated, out.FileCount)
	}
}

func TestCappedBufferStopsAtItsLimit(t *testing.T) {
	b := &cappedBuffer{limit: 10}

	n, err := b.Write([]byte("12345"))
	if n != 5 || err != nil || b.overflowed {
		t.Fatalf("under the limit: n=%d err=%v overflowed=%v", n, err, b.overflowed)
	}

	// Writes past the limit are absorbed, not errored: the command is already
	// doomed and returning an error here would surface as a write failure
	// rather than as the size problem it is.
	n, err = b.Write([]byte("678901234567890"))
	if err != nil {
		t.Fatalf("a write past the limit must be absorbed, got %v", err)
	}
	if n != 15 {
		t.Errorf("Write must report the full length it was given, got %d", n)
	}
	if !b.overflowed {
		t.Error("overflow must be recorded")
	}
	if b.Len() != 10 {
		t.Errorf("buffer grew to %d, past its %d limit - the allocation the ceiling exists to prevent", b.Len(), 10)
	}
}
