package arcade

import (
	"os"
	"strconv"
	"strings"
)

// memTotalMB reports the memory this panel may actually use.
//
// Two sources, and the smaller wins, because either can be the real ceiling:
//
//   - /proc/meminfo. Under Proxmox LXC this is namespaced by lxcfs, so it
//     already reports the container's limit rather than the hypervisor's RAM.
//     Without lxcfs it reports the host's, which is why it is not trusted alone.
//   - The cgroup memory limit. This is what the kernel actually enforces, and
//     in a Docker container it is the only one of the two that is true.
//
// A panel that reads only /proc/meminfo inside a 4 GB container on a 64 GB host
// will offer to allocate 64 GB. Taking the minimum is what makes the number
// mean "what I am allowed", which is the question the dashboard is asking.
func memTotalMB() int {
	best := 0
	consider := func(mb int) {
		if mb <= 0 {
			return
		}
		if best == 0 || mb < best {
			best = mb
		}
	}

	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(ln, "MemTotal:") {
				continue
			}
			f := strings.Fields(ln)
			if len(f) >= 2 {
				if kb, err := strconv.Atoi(f[1]); err == nil {
					consider(kb / 1024)
				}
			}
			break
		}
	}

	// cgroup v2, then v1. "max" means no limit, which is not a number.
	for _, p := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "max" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		// v1 writes a sentinel near max-int64 to mean unlimited. Anything past
		// a petabyte is that sentinel, not a machine.
		if n <= 0 || n > (1<<50) {
			continue
		}
		consider(int(n / (1 << 20)))
	}

	return best
}
