package devcouncil

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The bounds and seams of the commit content check: window overlap, the stream
// budget and deadline, and the limits on reading secret files. They tune the
// check through commitScanSeams, which is how a test reaches a bound without a
// gigabyte of fixture.

// A binary is found by its bytes wherever a window edge falls, including inside
// the value. The negative control proves the sweep could fail: with no overlap
// the same bytes are missed.
func TestWindowSinkFindsAValueAcrossEveryChunkEdge(t *testing.T) {
	needle := []windowNeedle{{reason: "value", bytes: []byte(secretValue)}}
	scan := func(chunk, overlap, pad int, feed int) bool {
		var found bool
		sink := &windowSink{
			ctx: context.Background(), label: "x.bin", chunk: chunk, overlap: overlap, values: needle,
			hit: func(string, string) { found = true },
		}
		data := []byte(strings.Repeat("\x00", pad) + secretValue + strings.Repeat("\x01", 40))
		for len(data) > 0 {
			n := min(feed, len(data))
			if _, err := sink.Write(data[:n]); err != nil {
				t.Fatal(err)
			}
			data = data[n:]
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, chunk := range []int{1, 7, 16, 33, 64} {
		for _, feed := range []int{1, 5, 32768} {
			for pad := 0; pad < 70; pad++ {
				if !scan(chunk, len(secretValue)-1, pad, feed) {
					t.Fatalf("chunk=%d feed=%d pad=%d: the value was missed", chunk, feed, pad)
				}
			}
		}
	}
	missed := false
	for pad := 0; pad < 70; pad++ {
		if !scan(16, 0, pad, 1) {
			missed = true
		}
	}
	if !missed {
		t.Fatal("with no overlap every offset was found, so the sweep proves nothing about the overlap")
	}
}

// The same, end to end through real git, for both kinds of needle: a value from
// a secret file, and a credential KnownSecret holds (sized by KnownSecretMaxLen).
func TestCommitFindsASecretAcrossAChunkEdgeInABinary(t *testing.T) {
	const token = "ghp_KNOWNtoken0123456789" //nolint:gosec // G101: a test fixture, not a credential
	t.Run("value from a secret file", func(t *testing.T) {
		f := scanFixture(t)
		f.reg.scan.limits.chunkBytes = 16
		write(t, f.root, "blob.bin", "\x00"+strings.Repeat("\x01", 21)+secretValue+"\x00\x02")
		requireLeak(t, stageAndCommit(t, f, "blob.bin"), "blob.bin", "contains a value assigned in .env")
	})
	t.Run("credential the process holds", func(t *testing.T) {
		f := scanFixture(t)
		f.reg.scan.limits.chunkBytes = 16
		f.reg.deps.KnownSecret = func(text string) bool { return strings.Contains(text, token) }
		f.reg.deps.KnownSecretMaxLen = func() int { return len(token) }
		write(t, f.root, "blob.bin", "\x00"+strings.Repeat("\x01", 21)+token+"\x00\x02")
		requireLeak(t, stageAndCommit(t, f, "blob.bin"), "blob.bin", "contains a credential this process holds")
	})
}

// A scan that cannot finish inside its budget says where it stopped.
func TestCommitRefusesWhenTheScanBudgetIsSpent(t *testing.T) {
	f := scanFixture(t)
	f.reg.scan.limits.streamBytes = 1000
	write(t, f.root, "big.bin", zeros(5000))
	res := stageAndCommit(t, f, "big.bin")
	requireCommitRefusedUnchecked(t, f, res, "the staged blob big.bin was not fully scanned", "stopped after 0 bytes", "1000-byte budget", "commit fewer binary files")
}

func TestCommitRefusesWhenTheScanDeadlineFires(t *testing.T) {
	f := scanFixture(t)
	f.reg.scan.limits.scanTimeout = 150 * time.Millisecond
	f.reg.scan.limits.chunkBytes = 64 << 10
	f.reg.deps.KnownSecret = func(string) bool { time.Sleep(30 * time.Millisecond); return false }
	write(t, f.root, "big.bin", zeros(4<<20))
	res := stageAndCommit(t, f, "big.bin")
	requireCommitRefusedUnchecked(t, f, res, "the scan of the staged blob big.bin was cut off by the deadline after", "commit fewer or smaller files")
}
