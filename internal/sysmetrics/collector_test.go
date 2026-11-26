package sysmetrics

import (
	"testing"
	"time"
)

func TestParseCPUStat(t *testing.T) {
	cs, err := parseCPUStat("cpu  100 0 0 900 0 0 0 0 0 0")
	if err != nil {
		t.Fatalf("parseCPUStat: %v", err)
	}
	if cs.idle != 900 {
		t.Fatalf("idle: got %d want 900", cs.idle)
	}
	if cs.total != 1000 {
		t.Fatalf("total: got %d want 1000", cs.total)
	}
}

func TestParseCPUStat_Bad(t *testing.T) {
	if _, err := parseCPUStat("cpu0 1 2 3 4"); err == nil {
		t.Fatal("expected error for cpu0 line")
	}
	if _, err := parseCPUStat(""); err == nil {
		t.Fatal("expected error for empty line")
	}
}

func TestCPUPercentFromDelta(t *testing.T) {
	prev := cpuStat{idle: 900, total: 1000}
	// idle 910/1100 → idleDelta=10, totalDelta=100 → 90%
	cur := cpuStat{idle: 910, total: 1100}
	got := cpuPercentFromDelta(prev, cur)
	if got < 89.9 || got > 90.1 {
		t.Fatalf("cpu percent: got %v want ~90", got)
	}
}

func TestCPUPercentClamp(t *testing.T) {
	// fully busy: idleDelta=0 → 100%
	busy := cpuPercentFromDelta(cpuStat{idle: 50, total: 100}, cpuStat{idle: 50, total: 200})
	if busy != 100 {
		t.Fatalf("fully busy: got %v want 100", busy)
	}
	// fully idle: idleDelta==totalDelta → 0%
	idle := cpuPercentFromDelta(cpuStat{idle: 50, total: 100}, cpuStat{idle: 150, total: 200})
	if idle != 0 {
		t.Fatalf("fully idle: got %v want 0", idle)
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo("MemTotal:       2048 kB\nMemAvailable:    512 kB\n")
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if total != 2048*1024 {
		t.Fatalf("total: got %d", total)
	}
	if avail != 512*1024 {
		t.Fatalf("avail: got %d", avail)
	}
}

func TestParseMeminfo_Missing(t *testing.T) {
	if _, _, err := parseMeminfo("MemTotal: 1000 kB\n"); err == nil {
		t.Fatal("expected error without MemAvailable")
	}
}

func TestParseDiskstats(t *testing.T) {
	data := "" +
		"  8       0 sda 1 2 1000 3 4 5 2000 6 7 8 9\n" +
		"  8       1 sda1 1 2 99999 3 4 5 99999 6 7 8 9\n" +
		"259       0 nvme0n1 1 2 500 3 4 5 700 6 7 8 9\n" +
		"259       1 nvme0n1p1 1 2 1 3 4 5 1 6 7 8 9\n" +
		"  7       0 loop0 1 2 50 3 4 5 50 6 7 8 9\n"
	ds, err := parseDiskstats(data)
	if err != nil {
		t.Fatalf("parseDiskstats: %v", err)
	}
	// sda + nvme0n1 only
	if ds.sectorsRead != 1000+500 {
		t.Fatalf("sectorsRead: got %d want 1500", ds.sectorsRead)
	}
	if ds.sectorsWrite != 2000+700 {
		t.Fatalf("sectorsWrite: got %d want 2700", ds.sectorsWrite)
	}
}

func TestIsWholeDisk(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"sda", true},
		{"sda1", false},
		{"vda", true},
		{"vda2", false},
		{"xvda", true},
		{"nvme0n1", true},
		{"nvme0n1p1", false},
		{"hda", true},
		{"hda1", false},
		{"loop0", false},
		{"dm-0", false},
	}
	for _, tc := range cases {
		got := isWholeDisk(tc.name)
		if got != tc.ok {
			t.Errorf("%s: isWholeDisk=%v want %v", tc.name, got, tc.ok)
		}
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg("0.10 0.20 0.30 1/100 1\n")
	if err != nil {
		t.Fatalf("parseLoadavg: %v", err)
	}
	if l1 != 0.10 || l5 != 0.20 || l15 != 0.30 {
		t.Fatalf("load: %v %v %v", l1, l5, l15)
	}
}

func TestParseUptime(t *testing.T) {
	u, err := parseUptime("123.45 999.99\n")
	if err != nil {
		t.Fatalf("parseUptime: %v", err)
	}
	if u != 123.45 {
		t.Fatalf("uptime: got %v", u)
	}
}

func TestSample_Live(t *testing.T) {
	c := New()
	snap1, err := c.Sample()
	if err != nil {
		t.Fatalf("first Sample: %v", err)
	}
	if snap1.CPUPercent != nil {
		t.Fatalf("first CPUPercent want nil, got %v", *snap1.CPUPercent)
	}
	if snap1.DiskReadBps != nil || snap1.DiskWriteBps != nil {
		t.Fatalf("first disk rates want nil")
	}
	if snap1.IntervalSec != 0 {
		t.Fatalf("first IntervalSec want 0, got %v", snap1.IntervalSec)
	}
	if snap1.MemTotalBytes == 0 {
		t.Fatal("MemTotalBytes should be > 0")
	}
	if snap1.DiskTotalBytes == 0 {
		t.Fatal("DiskTotalBytes should be > 0")
	}
	if snap1.UptimeSec <= 0 {
		t.Fatalf("UptimeSec: got %v", snap1.UptimeSec)
	}

	time.Sleep(50 * time.Millisecond)
	snap2, err := c.Sample()
	if err != nil {
		t.Fatalf("second Sample: %v", err)
	}
	if snap2.CPUPercent == nil {
		t.Fatal("second CPUPercent should be set")
	}
	if *snap2.CPUPercent < 0 || *snap2.CPUPercent > 100 {
		t.Fatalf("CPUPercent out of range: %v", *snap2.CPUPercent)
	}
	if snap2.DiskReadBps == nil || snap2.DiskWriteBps == nil {
		t.Fatal("second disk rates should be set")
	}
	if snap2.IntervalSec <= 0 {
		t.Fatalf("IntervalSec should be > 0, got %v", snap2.IntervalSec)
	}
}
