// Phase 0.2 spike — stress-test neutronrealtime.Hub under a console flood.
//
// DoD (PLAN.md §10 Phase 0.2): measured backpressure, no goroutine leaks,
// drop rate acceptable. Drives the Hub directly (no WS transport) so the
// numbers reflect the fanout/trySend core that the real console path depends on.
//
// Run:
//
//	go run .
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutron-dev/neutron-go/neutronrealtime"
)

const (
	bufSize       = 256
	payloadBytes  = 160
	floodDuration = 3 * time.Second
)

func makePayload(n int) []byte {
	b := make([]byte, payloadBytes)
	for i := range b {
		b[i] = 'a' + byte((n+i)%26)
	}
	return b
}

type viewer struct {
	conn      *neutronrealtime.Conn
	delivered atomic.Int64
}

func newViewer(hub *neutronrealtime.Hub, room, id string) *viewer {
	c := neutronrealtime.NewConn(id, bufSize)
	hub.Register(c)
	hub.Subscribe(room, c)
	v := &viewer{conn: c}
	go func() {
		for range c.Send {
			v.delivered.Add(1)
		}
	}()
	return v
}

func main() {
	fmt.Println("=== neutronrealtime Hub stress (console flood) ===")
	fmt.Printf("config: bufSize=%d payload=%dB flood=%s\n\n", bufSize, payloadBytes, floodDuration)

	room := "server:1"

	// Test 1: saturating flood, 10 viewers.
	testFlood(room, 10, 0)

	// Test 2: rate-limited 5k msg/s, 10 viewers (realistic heavy MC startup).
	testFlood(room, 10, 5000)

	// Test 3: rate-limited 5k msg/s, 100 viewers (stress).
	testFlood(room, 100, 5000)

	// Test 4: slow-consumer isolation.
	testSlowConsumer(room)

	// Test 5: goroutine leak.
	testLeak(room)

	fmt.Println("\nDoD check: see per-test verdicts above.")
}

func testFlood(room string, viewers int, targetHz int) {
	hub := neutronrealtime.NewHub()
	vs := make([]*viewer, viewers)
	for i := range vs {
		vs[i] = newViewer(hub, room, fmt.Sprintf("v%d", i))
	}

	payload := makePayload(0)
	var produced atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := (*time.Ticker)(nil)
		if targetHz > 0 {
			ticker = time.NewTicker(time.Second / time.Duration(targetHz))
			defer ticker.Stop()
		}
		for {
			if ticker != nil {
				select {
				case <-stop:
					return
				case <-ticker.C:
					hub.Broadcast(room, payload)
					produced.Add(1)
				}
			} else {
				select {
				case <-stop:
					return
				default:
					hub.Broadcast(room, payload)
					produced.Add(1)
				}
			}
		}
	}()

	start := time.Now()
	time.Sleep(floodDuration)
	close(stop)
	wg.Wait()
	prodWall := time.Since(start)

	drainSettleDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(drainSettleDeadline) {
		if minDelivered(vs) >= produced.Load()-int64(viewers) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(50 * time.Millisecond)

	prod := produced.Load()
	dmin, dmax, dsum := deliveredStats(vs)
	achievedHz := float64(prod) / prodWall.Seconds()
	dropRate := 1 - (float64(dsum) / float64(prod*int64(viewers)))

	label := fmt.Sprintf("saturating (max)")
	if targetHz > 0 {
		label = fmt.Sprintf("rate-limited %d Hz", targetHz)
	}
	fmt.Printf("--- %s, %d viewers ---\n", label, viewers)
	fmt.Printf("produced:        %d  (%.0f msg/s achieved)\n", prod, achievedHz)
	fmt.Printf("delivered/viewer: min=%d max=%d  (fairness spread=%d)\n", dmin, dmax, dmax-dmin)
	fmt.Printf("drop rate:       %.4f%%  (across all viewers)\n", dropRate*100)
	fmt.Printf("verdict:         %s\n", verdict(dropRate))
	fmt.Println()

	for _, v := range vs {
		hub.Unregister(v.conn)
	}
}

func testSlowConsumer(room string) {
	hub := neutronrealtime.NewHub()
	fast := make([]*viewer, 9)
	for i := range fast {
		fast[i] = newViewer(hub, room, fmt.Sprintf("f%d", i))
	}
	slow := neutronrealtime.NewConn("slow", bufSize)
	hub.Register(slow)
	hub.Subscribe(room, slow)

	payload := makePayload(1)
	const total = 20000
	start := time.Now()
	for i := 0; i < total; i++ {
		hub.Broadcast(room, payload)
	}
	broadcastWall := time.Since(start)
	time.Sleep(50 * time.Millisecond)

	fmin, fmax, fsum := deliveredStats(fast)
	slowDelivered := int64(0)
	remaining := 0
loop:
	for {
		select {
		case <-slow.Send:
			slowDelivered++
		default:
			remaining = len(slow.Send)
			break loop
		}
	}

	fmt.Println("--- slow-consumer isolation (1 of 10 viewers stalled) ---")
	fmt.Printf("produced:            %d\n", total)
	fmt.Printf("broadcast wall:      %v  (%.1f µs/broadcast over 10 viewers)\n",
		broadcastWall, float64(broadcastWall.Microseconds())/float64(total))
	fmt.Printf("fast viewers:        min=%d max=%d  (sum=%d, expected=%d)\n", fmin, fmax, fsum, total*9)
	fmt.Printf("slow viewer:         delivered=%d (buffer cap=%d, held=%d)\n", slowDelivered, bufSize, remaining)
	blocked := broadcastWall > 5*time.Second
	fastFull := fsum == int64(total*9)
	noPanic := true
	fmt.Printf("verdict:             %s\n", slowVerdict(blocked, fastFull, noPanic, slowDelivered))
	fmt.Println()

	hub.Unregister(slow)
	for _, v := range fast {
		hub.Unregister(v.conn)
	}
}

func testLeak(room string) {
	before := runtime.NumGoroutine()
	const n = 200
	hub := neutronrealtime.NewHub()
	vs := make([]*viewer, n)
	for i := range vs {
		vs[i] = newViewer(hub, room, fmt.Sprintf("l%d", i))
	}
	peak := runtime.NumGoroutine()
	for i := 0; i < 1000; i++ {
		hub.Broadcast(room, makePayload(i))
	}
	for _, v := range vs {
		hub.Unregister(v.conn)
	}
	time.Sleep(300 * time.Millisecond)
	after := runtime.NumGoroutine()

	fmt.Println("--- goroutine leak (200 conns registered then unregistered) ---")
	fmt.Printf("before: %d  peak: %d  after: %d  (delta after-before = %d)\n", before, peak, after, after-before)
	leaked := after-before > 0
	fmt.Printf("verdict: %s\n", leakVerdict(leaked))
	fmt.Println()
}

func minDelivered(vs []*viewer) int64 {
	m := vs[0].delivered.Load()
	for _, v := range vs[1:] {
		if d := v.delivered.Load(); d < m {
			m = d
		}
	}
	return m
}

func deliveredStats(vs []*viewer) (int64, int64, int64) {
	var min, max, sum int64
	min = vs[0].delivered.Load()
	max = min
	for _, v := range vs {
		d := v.delivered.Load()
		sum += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	return min, max, sum
}

func verdict(dropRate float64) string {
	switch {
	case dropRate == 0:
		return "PASS — zero drops"
	case dropRate < 0.001:
		return fmt.Sprintf("PASS — negligible drops (%.4f%%)", dropRate*100)
	case dropRate < 0.05:
		return fmt.Sprintf("ACCEPTABLE — %.2f%% dropped (backpressure shedding working)", dropRate*100)
	default:
		return fmt.Sprintf("HIGH — %.2f%% dropped (buffer too small or producer too fast)", dropRate*100)
	}
}

func slowVerdict(blocked, fastFull, noPanic bool, slowDelivered int64) string {
	switch {
	case !noPanic:
		return "FAIL — panicked"
	case blocked:
		return "FAIL — broadcaster blocked"
	case !fastFull:
		return "FAIL — slow consumer starved fast viewers"
	case slowDelivered > int64(bufSize):
		return "FAIL — slow consumer exceeded buffer (no backpressure)"
	default:
		return "PASS — broadcaster never blocked, fast viewers got 100%, slow consumer capped at buffer"
	}
}

func leakVerdict(leaked bool) string {
	if leaked {
		return "FAIL — goroutines leaked after Unregister"
	}
	return "PASS — all drain goroutines exited (no leak)"
}
