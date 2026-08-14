package arcade

import (
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
