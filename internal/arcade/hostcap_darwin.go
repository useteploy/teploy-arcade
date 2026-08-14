package arcade

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// memTotalMB reports physical memory on macOS.
//
// Shelling out to sysctl rather than adding golang.org/x/sys: this runs once at
// startup, and the dependency list is deliberately one entry long. macOS is the
// development platform, not a deployment target, so being exactly right here
// matters less than not paying for it everywhere else.
func memTotalMB() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n / (1 << 20))
}

// memUsedMB is best-effort on macOS: the development platform, not a
// deployment target.
func memUsedMB() int {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0
	}
	total, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || total <= 0 {
		return 0
	}
	// vm_stat reports free/inactive pages; anything else is in use.
	vs, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0
	}
	pageSize, freePages := int64(4096), int64(0)
	for _, ln := range strings.Split(string(vs), "\n") {
		if strings.HasPrefix(ln, "Mach Virtual Memory Statistics") {
			if i := strings.Index(ln, "page size of "); i >= 0 {
				fmt.Sscanf(ln[i+len("page size of "):], "%d", &pageSize)
			}
			continue
		}
		for _, k := range []string{"Pages free:", "Pages inactive:", "Pages speculative:"} {
			if strings.HasPrefix(ln, k) {
				var n int64
				fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(ln, k)), "%d", &n)
				freePages += n
			}
		}
	}
	used := total - freePages*pageSize
	if used < 0 {
		return 0
	}
	return int(used / (1 << 20))
}

type cpuSample struct{ busy, total uint64 }

func readCPUSample() (cpuSample, bool) { return cpuSample{}, false }
