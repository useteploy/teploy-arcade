package arcade

import (
	"strings"
	"testing"
)

// DiskGB was decoration: stored, summed into the "committed" figure, shown on
// screen, and enforced nowhere. TODO 8e called for XFS project quotas, which
// this host cannot have - it is ext4 inside an LXC. So the panel promises the
// part it can keep: it refuses to hand out space the disk does not have.
//
// The refusal is on free space, not on commitment, and that distinction is the
// whole point of these tests. The deployed host has 87 GB committed of 99 GB
// while using 25 GB; a commitment rule would refuse a 15 GB Forge server with
// 74 GB genuinely free.

func TestCreateIsRefusedWhenTheDiskCannotHoldIt(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Derived from the real reading so the arithmetic is stable on any machine:
	// a total that leaves exactly 4 GB free.
	restore := hostDiskGB
	t.Cleanup(func() { hostDiskGB = restore })
	hostDiskGB = diskUsedGB(mgr.dataDir) + 4

	err := mgr.checkDiskSpace(10)
	if err == nil {
		t.Fatal("a 10 GB server was allowed onto a disk with 4 GB free")
	}
	if !strings.Contains(err.Error(), "4 GB free") {
		t.Errorf("the refusal should say how much is free; got %q", err)
	}

	if err := mgr.checkDiskSpace(4); err != nil {
		t.Fatalf("a server that exactly fits was refused: %v", err)
	}
}

// Over-commitment is normal and must stay allowed: servers are given
// allowances they will mostly never fill, and refusing on the sum would block
// creates on a host that is three-quarters empty.
func TestOvercommitmentAloneDoesNotBlockACreate(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	restore := hostDiskGB
	t.Cleanup(func() { hostDiskGB = restore })
	hostDiskGB = diskUsedGB(mgr.dataDir) + 40 // 40 GB genuinely free

	// Commit far more than the disk holds, the way a fleet of templated
	// servers does.
	for _, s := range mgr.List() {
		s.mu.Lock()
		s.DiskGB = 500
		s.mu.Unlock()
	}

	if err := mgr.checkDiskSpace(10); err != nil {
		t.Fatalf("a 10 GB server was refused with 40 GB free, on commitment alone: %v", err)
	}
}

// A host that cannot report its disk size must not become a host that refuses
// every create.
func TestUnknownDiskSizeDoesNotBlockCreates(t *testing.T) {
	mgr := NewManager(t.TempDir(), NewHub())
	if err := mgr.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	restore := hostDiskGB
	t.Cleanup(func() { hostDiskGB = restore })
	hostDiskGB = 0

	if err := mgr.checkDiskSpace(500); err != nil {
		t.Fatalf("an unknown disk size refused a create: %v", err)
	}
}
