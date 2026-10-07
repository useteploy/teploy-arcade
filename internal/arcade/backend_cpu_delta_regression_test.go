package arcade

import "testing"

func TestBackendCPUUsagePercent(t *testing.T) {
	for _, tc := range []struct {
		name string
		prev cpuSample
		cur  cpuSample
		want float64
		ok   bool
	}{
		{"half busy", cpuSample{50, 100}, cpuSample{100, 200}, 50, true},
		{"idle", cpuSample{50, 100}, cpuSample{50, 200}, 0, true},
		{"fully busy", cpuSample{50, 100}, cpuSample{150, 200}, 100, true},
		{"first sample", cpuSample{}, cpuSample{50, 100}, 0, false},
		{"no interval", cpuSample{50, 100}, cpuSample{50, 100}, 0, false},
		{"counter reset", cpuSample{50, 100}, cpuSample{5, 10}, 0, false},
		{"busy reset only", cpuSample{50, 100}, cpuSample{5, 200}, 0, false},
		{"total wrap", cpuSample{10, ^uint64(0)}, cpuSample{20, 1}, 0, false},
		{"busy exceeds interval", cpuSample{10, 100}, cpuSample{90, 110}, 0, false},
		{"invalid previous sample", cpuSample{101, 100}, cpuSample{110, 200}, 0, false},
		{"invalid current sample", cpuSample{50, 100}, cpuSample{201, 200}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := cpuUsagePercent(tc.prev, tc.cur)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("cpuUsagePercent = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
