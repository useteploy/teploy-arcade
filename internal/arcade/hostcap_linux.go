package arcade

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	for _, p := range cgroupMemoryPaths("memory.max", "memory.limit_in_bytes", true) {
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

// memUsedMB reports usage from /proc/meminfo. This is host/LXC memory, not
// necessarily the cgroup-limited capacity returned by memTotalMB (R45).
// MemAvailable includes reclaimable page cache; MemFree alone does not.
func memUsedMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	used, _ := parseMemUsedMB(string(b))
	return used
}

// parseMemUsedMB separates unavailable/malformed fields from valid zero
// availability. Subtract in kB before converting to whole MiB; rounding each
// field first loses the near-exhausted state and can overstate usage.
func parseMemUsedMB(meminfo string) (int, bool) {
	var total, avail uint64
	var haveTotal, haveAvail bool
	for _, ln := range strings.Split(meminfo, "\n") {
		f := strings.Fields(ln)
		if len(f) == 0 || (f[0] != "MemTotal:" && f[0] != "MemAvailable:") {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return 0, false
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return 0, false
		}
		switch f[0] {
		case "MemTotal:":
			if haveTotal {
				return 0, false
			}
			total, haveTotal = n, true
		case "MemAvailable:":
			if haveAvail {
				return 0, false
			}
			avail, haveAvail = n, true
		}
	}
	if !haveTotal || !haveAvail || total == 0 || avail > total {
		return 0, false
	}
	used := (total - avail) / 1024
	if used > uint64(^uint(0)>>1) {
		return 0, false
	}
	return int(used), true
}

// cpuSample is one reading of the host's cumulative CPU counters.
type cpuSample struct{ busy, total uint64 }

// readCPUSample reads /proc/stat's aggregate line. CPU usage is a rate, not a
// level, so it can only be derived from two readings - a single one says
// nothing.
func readCPUSample() (cpuSample, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	return parseCPUSample(string(b))
}

func parseCPUSample(stat string) (cpuSample, bool) {
	for _, ln := range strings.Split(stat, "\n") {
		fields := strings.Fields(ln)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		// user, nice, system and idle are required. Linux appends optional
		// iowait, irq, softirq, steal, guest and guest_nice counters.
		if len(fields) < 5 || len(fields) > 11 {
			return cpuSample{}, false
		}
		var counters [10]uint64
		for i, f := range fields[1:] {
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			counters[i] = n
		}
		// guest/guest_nice are already included in user/nice by the kernel.
		// Including them again inflates both busy and total CPU time.
		if counters[8] > counters[0] || counters[9] > counters[1] {
			return cpuSample{}, false
		}
		var s cpuSample
		for i, n := range counters[:8] {
			if n > ^uint64(0)-s.total {
				return cpuSample{}, false
			}
			s.total += n
			// Keep the existing policy: idle/iowait are non-busy; steal is
			// busy/unavailable host capacity, alongside user/system/IRQs.
			if i != 3 && i != 4 {
				s.busy += n
			}
		}
		return s, s.total > 0
	}
	return cpuSample{}, false
}

// Usage carries its own scope instead of pairing physical /proc usage with a
// cgroup-limited capacity. Unavailable readings are null, never synthetic zero.
func memoryUsageView() map[string]any {
	for _, path := range cgroupMemoryPaths("memory.current", "memory.usage_in_bytes", false) {
		if b, err := os.ReadFile(path); err == nil {
			if n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				return map[string]any{"used_mb": int(n / (1 << 20)), "scope": "panel_cgroup", "source": path, "sampled_at": time.Now()}
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if used, ok := parseMemUsedMB(string(b)); ok {
			return map[string]any{"used_mb": used, "scope": "proc_visible_host", "source": "/proc/meminfo", "sampled_at": time.Now()}
		}
	}
	return map[string]any{"used_mb": nil, "scope": "unknown", "source": nil, "sampled_at": nil}
}

func cgroupMemoryPaths(v2, v1 string, ancestors bool) []string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		base, name := "/sys/fs/cgroup", v2
		if fields[0] != "0" {
			if !strings.Contains(","+fields[1]+",", ",memory,") {
				continue
			}
			base = "/sys/fs/cgroup/memory"
			name = v1
		}
		rel := strings.TrimPrefix(filepath.Clean(fields[2]), "/")
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		dir := filepath.Join(base, rel)
		for {
			paths = append(paths, filepath.Join(dir, name))
			if !ancestors || dir == base {
				break
			}
			next := filepath.Dir(dir)
			if next == dir || !strings.HasPrefix(next, base) {
				break
			}
			dir = next
		}
	}
	return paths
}
