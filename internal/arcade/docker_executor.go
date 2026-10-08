package arcade

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

var dockerCommandSlots = make(chan struct{}, 8)
var dockerWaitSlots = make(chan struct{}, 64)

type boundedCommandBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := (1 << 20) - b.buffer.Len()
	if remain < len(p) {
		b.exceeded = true
		p = p[:remain]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

// All finite daemon commands share admission, deadlines and bounded output.
// Supervised long-lived log streams use their separate manager-owned context.
func dockerOutput(parent context.Context, timeout time.Duration, combined bool, args ...string) ([]byte, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	slots := dockerCommandSlots
	if len(args) > 0 && args[0] == "wait" {
		slots = dockerWaitSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = time.Second
	var out, stderr boundedCommandBuffer
	cmd.Stdout = &out
	if combined {
		cmd.Stderr = &out
	} else {
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	if !combined && err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			e.Stderr = append([]byte(nil), stderr.buffer.Bytes()...)
		}
	}
	if out.exceeded || stderr.exceeded {
		return out.buffer.Bytes(), fmt.Errorf("Docker command output exceeded 1 MiB")
	}
	if ctx.Err() != nil {
		return out.buffer.Bytes(), ctx.Err()
	}
	return out.buffer.Bytes(), err
}
