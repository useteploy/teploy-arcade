package arcade

import (
	"syscall"
)

// Host capacity: what this machine actually has.
//
// These were hardcoded to 16384 MB and 200 GB, which is fine for a mockup and
// actively harmful on a real host. The dashboard shows allocated-versus-total,
// and the create screen refuses to size a server past the total, so an invented
// number is not cosmetic - on a 4 GB LXC reporting 16 GB, the panel cheerfully
// approves four times the memory the box has, and the kernel resolves the
// disagreement by killing whichever server is mid-chunk-generation.
//
// Detection is per-platform (see hostcap_linux.go / hostcap_darwin.go). When a
// value cannot be determined it is reported as 0, meaning "unknown", and the UI
// says so rather than inventing a figure. A wrong number is worse than no
// number here, because a wrong one still looks like an answer.

// diskTotalGB reports the size of the filesystem holding dir.
//
// The data directory rather than "/" on purpose: server files, worlds and
// backups all land there, and on any host where it is a separate mount - which
// is the normal arrangement for a machine that stores worlds - the root
// filesystem's size says nothing about the space this panel can use.
func diskTotalGB(dir string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	// Bsize is int64 on Linux and uint32 on Darwin; widen both.
	total := uint64(st.Blocks) * uint64(st.Bsize)
	return int(total / (1 << 30))
}

// diskUsedGB reports how much of the filesystem holding dir is in use.
//
// Bavail, not Bfree: the reserved blocks only root can touch are not space the
// panel or a game server can ever use, so counting them as free would promise
// room that does not exist.
func diskUsedGB(dir string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	bs := uint64(st.Bsize)
	total := uint64(st.Blocks) * bs
	avail := uint64(st.Bavail) * bs
	if total < avail {
		return 0
	}
	return int((total - avail) / (1 << 30))
}
