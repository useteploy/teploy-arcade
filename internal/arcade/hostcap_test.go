package arcade

import (
	"testing"
)

// These were hardcoded to 16384 MB and 200 GB. On the first real host the panel
// ran on - a 4 GB Proxmox LXC with a 50 GB disk - that meant the dashboard
// offered four times the memory the box had, and the create screen approved it.
// The test that matters is therefore not "is it exact" but "is it measured":
// a plausible constant is the failure mode.
func TestHostCapacityIsMeasuredNotAssumed(t *testing.T) {
	mem := memTotalMB()
	if mem <= 0 {
		t.Fatalf("memTotalMB() = %d; the running host has memory, so this is a detection failure", mem)
	}
	// The old constants. Landing exactly on one is possible but overwhelmingly
	// likely to mean the hardcoded value came back.
	if mem == 16384 {
		t.Errorf("memTotalMB() returned exactly the old hardcoded 16384 MB")
	}
	// Sanity: somewhere between a Pi and a large server.
	if mem < 256 || mem > 4<<20 {
		t.Errorf("memTotalMB() = %d MB, which is not a plausible machine", mem)
	}

	disk := diskTotalGB(t.TempDir())
	if disk <= 0 {
		t.Fatalf("diskTotalGB() = %d; the temp directory lives on a real filesystem", disk)
	}
	if disk == 200 {
		t.Errorf("diskTotalGB() returned exactly the old hardcoded 200 GB")
	}
}

// An unreadable path reports unknown rather than guessing. The UI renders 0 as
// "unknown"; a fallback constant would render as fact.
func TestUnmeasurableDiskReportsUnknown(t *testing.T) {
	if got := diskTotalGB("/definitely/not/a/real/path/on/this/host"); got != 0 {
		t.Errorf("diskTotalGB on a missing path = %d, want 0 (unknown)", got)
	}
}

// Host() must surface the measured values, not recompute or default them.
func TestHostReportsTheMeasuredCapacity(t *testing.T) {
	_, mgr := newTestAgent(t)
	h := mgr.Host()

	mem, ok := h["memory"].(map[string]any)
	if !ok {
		t.Fatalf("memory block missing from Host(): %#v", h["memory"])
	}
	if mem["total_mb"] != hostMemMB {
		t.Errorf("Host() reports total_mb=%v but the measured value is %d", mem["total_mb"], hostMemMB)
	}
	disk, ok := h["disk"].(map[string]any)
	if !ok {
		t.Fatalf("disk block missing from Host(): %#v", h["disk"])
	}
	if disk["total_gb"] != hostDiskGB {
		t.Errorf("Host() reports total_gb=%v but the measured value is %d", disk["total_gb"], hostDiskGB)
	}
}
