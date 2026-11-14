package turso_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type soakOptions struct {
	Duration  time.Duration
	Rate      float64
	Artifacts string
	Lanes     []string
	IdleConns int
	Backups   bool
	Cycle     time.Duration
}

func readSoakOptions() (soakOptions, error) {
	o := soakOptions{Rate: 100, Lanes: []string{"restarted", "long-lived"}, IdleConns: 2, Backups: true, Cycle: 30 * time.Second}
	var err error
	if o.Duration, err = time.ParseDuration(os.Getenv("BIRALOBASE_SOAK_DURATION")); err != nil || o.Duration <= 0 {
		return o, fmt.Errorf("invalid soak duration")
	}
	if raw := os.Getenv("BIRALOBASE_SOAK_WRITES_PER_SECOND"); raw != "" {
		if o.Rate, err = strconv.ParseFloat(raw, 64); err != nil || o.Rate < 0 || math.IsNaN(o.Rate) || math.IsInf(o.Rate, 0) {
			return o, fmt.Errorf("invalid soak write rate")
		}
	}
	o.Artifacts = os.Getenv("BIRALOBASE_SOAK_DIR")
	if o.Artifacts != "" && (!filepath.IsAbs(o.Artifacts) || strings.ContainsAny(o.Artifacts, "'?#")) {
		return o, fmt.Errorf("soak artifact directory must be absolute and contain no quote, ? or #")
	}
	if raw := os.Getenv("BIRALOBASE_SOAK_LANES"); raw != "" {
		o.Lanes = strings.Split(raw, ",")
		seen := map[string]bool{}
		for _, lane := range o.Lanes {
			if (lane != "restarted" && lane != "long-lived") || seen[lane] {
				return o, fmt.Errorf("invalid or duplicate soak lane %q", lane)
			}
			seen[lane] = true
		}
	}
	if raw := os.Getenv("BIRALOBASE_SOAK_MAX_IDLE_CONNS"); raw != "" {
		if o.IdleConns, err = strconv.Atoi(raw); err != nil || o.IdleConns < 0 || o.IdleConns > 8 {
			return o, fmt.Errorf("soak idle connections must be between 0 and 8")
		}
	}
	if raw := os.Getenv("BIRALOBASE_SOAK_BACKUPS"); raw != "" {
		if o.Backups, err = strconv.ParseBool(raw); err != nil {
			return o, fmt.Errorf("invalid soak backups flag")
		}
	}
	if raw := os.Getenv("BIRALOBASE_SOAK_CYCLE"); raw != "" {
		if o.Cycle, err = time.ParseDuration(raw); err != nil || o.Cycle <= 0 {
			return o, fmt.Errorf("invalid soak cycle duration")
		}
	}
	return o, nil
}

// RSS is process residency, not Go's virtual reservation and not cgroup memory
// (which also includes file cache). Zero means current RSS is unavailable.
func currentRSS() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

func monitorSoak(t *testing.T, dir string, writes *atomic.Int64) func() {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "metrics.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	encoder := json.NewEncoder(f)
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		value := struct {
			UTC        string  `json:"utc"`
			Seconds    float64 `json:"seconds"`
			Writes     int64   `json:"acknowledgedInserts"`
			RSS        uint64  `json:"rssBytes,omitempty"`
			PeakRSS    int64   `json:"peakRSSBytes"`
			HeapAlloc  uint64  `json:"goHeapAllocBytes"`
			HeapInuse  uint64  `json:"goHeapInuseBytes"`
			GoSys      uint64  `json:"goReservedBytes"`
			GCs        uint32  `json:"goGCs"`
			Goroutines int     `json:"goroutines"`
		}{time.Now().UTC().Format(time.RFC3339Nano), time.Since(started).Seconds(), writes.Load(), currentRSS(), peakRSS(), m.HeapAlloc, m.HeapInuse, m.Sys, m.NumGC, runtime.NumGoroutine()}
		if err := encoder.Encode(value); err != nil {
			t.Errorf("write soak metrics: %v", err)
		}
		t.Logf("resident: writes=%d rss=%dKiB peak=%dKiB go_heap=%dKiB go_inuse=%dKiB goroutines=%d", value.Writes, value.RSS>>10, value.PeakRSS>>10, value.HeapAlloc>>10, value.HeapInuse>>10, value.Goroutines)
	}
	profile := func(name, kind string, debug int) {
		path := filepath.Join(dir, name)
		out, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			t.Errorf("create soak profile: %v", err)
			return
		}
		err = pprof.Lookup(kind).WriteTo(out, debug)
		closeErr := out.Close()
		if err != nil || closeErr != nil {
			t.Errorf("write soak profile: %v %v", err, closeErr)
		} else if err = os.Rename(path+".tmp", path); err != nil {
			t.Errorf("replace soak profile: %v", err)
		}
	}
	sample()
	profile("heap-start.pprof", "heap", 0)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer f.Close()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for samples := 0; ; {
			select {
			case <-ticker.C:
				sample()
				samples++
				if samples%5 == 0 {
					profile("heap-latest.pprof", "heap", 0)
					profile("goroutines-latest.txt", "goroutine", 2)
				}
			case <-stop:
				sample()
				profile("heap-final.pprof", "heap", 0)
				profile("goroutines-final.txt", "goroutine", 2)
				return
			}
		}
	}()
	return func() { close(stop); <-done }
}

func TestSoakOptions(t *testing.T) {
	for _, key := range []string{"DURATION", "WRITES_PER_SECOND", "DIR", "LANES", "MAX_IDLE_CONNS", "BACKUPS", "CYCLE"} {
		t.Setenv("BIRALOBASE_SOAK_"+key, "")
	}
	t.Setenv("BIRALOBASE_SOAK_DURATION", "1m")
	o, err := readSoakOptions()
	if err != nil || o.Rate != 100 || len(o.Lanes) != 2 || o.IdleConns != 2 || !o.Backups || o.Cycle != 30*time.Second {
		t.Fatalf("default gate changed: %+v, %v", o, err)
	}
	for _, test := range []struct{ key, value string }{
		{"DURATION", "0s"}, {"WRITES_PER_SECOND", "NaN"}, {"WRITES_PER_SECOND", "+Inf"},
		{"DIR", "relative"}, {"LANES", "long-lived,long-lived"}, {"LANES", "unknown"},
		{"MAX_IDLE_CONNS", "9"}, {"BACKUPS", "sometimes"}, {"CYCLE", "0s"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			t.Setenv("BIRALOBASE_SOAK_"+test.key, test.value)
			if _, err := readSoakOptions(); err == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
}
