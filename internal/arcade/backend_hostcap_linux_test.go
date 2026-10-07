package arcade

import (
	"fmt"
	"testing"
)

func TestBackendLinuxMemUsedPrecision(t *testing.T) {
	for _, tc := range []struct {
		avail uint64
		want  int
	}{
		{0, 8192},
		{1, 8191},
		{512, 8191},
		{1023, 8191},
		{1024, 8191},
		{1025, 8190},
		{4194304, 4096},
		{8388608, 0},
	} {
		t.Run(fmt.Sprintf("available_%d_kB", tc.avail), func(t *testing.T) {
			input := fmt.Sprintf("MemTotal: 8388608 kB\nMemAvailable: %d kB\n", tc.avail)
			if got, ok := parseMemUsedMB(input); !ok || got != tc.want {
				t.Fatalf("parseMemUsedMB = (%d, %v), want (%d, true)", got, ok, tc.want)
			}
		})
	}
}

func TestBackendLinuxMemUsedValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  int
		ok    bool
	}{
		{"missing all", "", 0, false},
		{"missing total", "MemAvailable: 0 kB\n", 0, false},
		{"missing available", "MemTotal: 8388608 kB\n", 0, false},
		{"zero total", "MemTotal: 0 kB\nMemAvailable: 0 kB\n", 0, false},
		{"above total", "MemTotal: 1024 kB\nMemAvailable: 1025 kB\n", 0, false},
		{"bad total", "MemTotal: nope kB\nMemAvailable: 0 kB\n", 0, false},
		{"bad available", "MemTotal: 1024 kB\nMemAvailable: nope kB\n", 0, false},
		{"negative", "MemTotal: 1024 kB\nMemAvailable: -1 kB\n", 0, false},
		{"overflow", "MemTotal: 18446744073709551616 kB\nMemAvailable: 0 kB\n", 0, false},
		{"wrong unit", "MemTotal: 1024 MB\nMemAvailable: 0 kB\n", 0, false},
		{"missing unit", "MemTotal: 1024\nMemAvailable: 0 kB\n", 0, false},
		{"extra field", "MemTotal: 1024 kB extra\nMemAvailable: 0 kB\n", 0, false},
		{"duplicate total", "MemTotal: 1024 kB\nMemTotal: 2048 kB\nMemAvailable: 0 kB\n", 0, false},
		{"duplicate available", "MemTotal: 1024 kB\nMemAvailable: 0 kB\nMemAvailable: 1 kB\n", 0, false},
		{"unrelated field ignored", "Other: invalid\nMemAvailable: 512 kB\nMemTotal: 2048 kB\n", 1, true},
		{"subtract before rounding", "MemTotal: 1536 kB\nMemAvailable: 1023 kB\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMemUsedMB(tc.input)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseMemUsedMB = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBackendLinuxCPUSampleAccounting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  cpuSample
		ok    bool
	}{
		{"guest included in user", "cpu 50 0 0 50 0 0 0 0 50 0\n", cpuSample{50, 100}, true},
		{"guest nice included in nice", "cpu 0 50 0 50 0 0 0 0 0 50\n", cpuSample{50, 100}, true},
		{"full fields", "cpu 100 20 30 50 10 4 6 5 25 10\n", cpuSample{165, 225}, true},
		{"old four field format", "cpu 10 5 15 70\n", cpuSample{30, 100}, true},
		{"iowait remains idle", "cpu 10 0 0 10 80\n", cpuSample{10, 100}, true},
		{"steal remains busy", "cpu 10 0 0 50 0 0 0 40\n", cpuSample{50, 100}, true},
		{"tabs and per CPU lines", "cpu0 999 0 0 0\ncpu\t50 0 0 50\n", cpuSample{50, 100}, true},
		{"missing aggregate", "cpu0 50 0 0 50\n", cpuSample{}, false},
		{"all zero", "cpu 0 0 0 0\n", cpuSample{}, false},
		{"too few fields", "cpu 10 20 30\n", cpuSample{}, false},
		{"unknown extra fields", "cpu 10 0 0 20 0 0 0 0 0 0 99\n", cpuSample{}, false},
		{"malformed field", "cpu 10 bad 0 20\n", cpuSample{}, false},
		{"malformed guest", "cpu 10 0 0 20 0 0 0 0 bad 0\n", cpuSample{}, false},
		{"negative", "cpu 10 0 0 -20\n", cpuSample{}, false},
		{"counter overflow", "cpu 18446744073709551616 0 0 1\n", cpuSample{}, false},
		{"total overflow", "cpu 18446744073709551615 0 0 1\n", cpuSample{}, false},
		{"guest exceeds user", "cpu 10 0 0 20 0 0 0 0 11 0\n", cpuSample{}, false},
		{"guest nice exceeds nice", "cpu 10 0 0 20 0 0 0 0 0 1\n", cpuSample{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCPUSample(tc.input)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseCPUSample = (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
