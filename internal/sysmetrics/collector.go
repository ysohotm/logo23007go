package sysmetrics

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Snapshot is returned by Collector.Sample.
type Snapshot struct {
	Time           time.Time `json:"time"`
	CPUPercent     *float64  `json:"cpu_percent"` // nil on first sample
	MemTotalBytes  uint64    `json:"mem_total_bytes"`
	MemUsedBytes   uint64    `json:"mem_used_bytes"` // total - available (MemAvailable)
	MemPercent     float64   `json:"mem_percent"`
	DiskReadBps    *float64  `json:"disk_read_bps"` // nil first sample; aggregated whole disks
	DiskWriteBps   *float64  `json:"disk_write_bps"`
	DiskTotalBytes uint64    `json:"disk_total_bytes"` // root filesystem (/)
	DiskUsedBytes  uint64    `json:"disk_used_bytes"`
	DiskPercent    float64   `json:"disk_percent"`
	Load1          float64   `json:"load1"`
	Load5          float64   `json:"load5"`
	Load15         float64   `json:"load15"`
	UptimeSec      float64   `json:"uptime_sec"`
	IntervalSec    float64   `json:"interval_sec"` // delta since previous sample; 0 first
}

var wholeDiskRE = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|hd[a-z]+)$`)

type cpuStat struct {
	idle  uint64
	total uint64
}

type diskStat struct {
	sectorsRead  uint64
	sectorsWrite uint64
}

// Collector keeps previous CPU/disk counters for rate calculation. Safe for concurrent Sample.
type Collector struct {
	mu sync.Mutex

	havePrev bool
	prevCPU  cpuStat
	prevDisk diskStat
	prevTime time.Time
}

// New returns a Collector that reads live /proc and root filesystem stats.
func New() *Collector {
	return &Collector{}
}

// Sample reads /proc and returns a snapshot. First call: rate fields nil. Later: rates from delta.
func (c *Collector) Sample() (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	snap := Snapshot{Time: now}

	statData, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Snapshot{}, err
	}
	firstLine, _, _ := strings.Cut(string(statData), "\n")
	cpu, err := parseCPUStat(firstLine)
	if err != nil {
		return Snapshot{}, err
	}

	memData, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Snapshot{}, err
	}
	memTotal, memAvail, err := parseMeminfo(string(memData))
	if err != nil {
		return Snapshot{}, err
	}
	snap.MemTotalBytes = memTotal
	snap.MemUsedBytes = memTotal - memAvail
	if memTotal > 0 {
		snap.MemPercent = 100 * float64(snap.MemUsedBytes) / float64(memTotal)
	}

	diskData, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return Snapshot{}, err
	}
	disk, err := parseDiskstats(string(diskData))
	if err != nil {
		return Snapshot{}, err
	}

	fsTotal, fsUsed, err := readRootFS()
	if err != nil {
		return Snapshot{}, err
	}
	snap.DiskTotalBytes = fsTotal
	snap.DiskUsedBytes = fsUsed
	if fsTotal > 0 {
		snap.DiskPercent = 100 * float64(fsUsed) / float64(fsTotal)
	}

	loadData, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return Snapshot{}, err
	}
	l1, l5, l15, err := parseLoadavg(string(loadData))
	if err != nil {
		return Snapshot{}, err
	}
	snap.Load1, snap.Load5, snap.Load15 = l1, l5, l15

	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return Snapshot{}, err
	}
	uptime, err := parseUptime(string(uptimeData))
	if err != nil {
		return Snapshot{}, err
	}
	snap.UptimeSec = uptime

	if c.havePrev {
		dt := now.Sub(c.prevTime).Seconds()
		snap.IntervalSec = dt
		if dt > 0 {
			totalDelta := cpu.total - c.prevCPU.total
			idleDelta := cpu.idle - c.prevCPU.idle
			pct := 0.0
			if totalDelta > 0 {
				pct = (1 - float64(idleDelta)/float64(totalDelta)) * 100
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
			}
			snap.CPUPercent = &pct
			const sectorBytes = 512.0
			readBps := float64(disk.sectorsRead-c.prevDisk.sectorsRead) * sectorBytes / dt
			writeBps := float64(disk.sectorsWrite-c.prevDisk.sectorsWrite) * sectorBytes / dt
			snap.DiskReadBps = &readBps
			snap.DiskWriteBps = &writeBps
		}
	}

	c.prevCPU = cpu
	c.prevDisk = disk
	c.prevTime = now
	c.havePrev = true

	return snap, nil
}

func readRootFS() (total, used uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	used = (st.Blocks - st.Bfree) * bsize
	return total, used, nil
}

// parseCPUStat parses the aggregate "cpu " line from /proc/stat.
func parseCPUStat(line string) (cpuStat, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuStat{}, fmt.Errorf("sysmetrics: unexpected /proc/stat line: %q", line)
	}
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		v, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return cpuStat{}, fmt.Errorf("sysmetrics: parse /proc/stat field %d: %w", i, err)
		}
		total += v
		if i == 4 { // idle
			idle = v
		}
	}
	return cpuStat{idle: idle, total: total}, nil
}

// parseMeminfo returns MemTotal and MemAvailable in bytes.
func parseMeminfo(data string) (total, available uint64, err error) {
	var memTotal, memAvailable uint64
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(data, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, perr := strconv.ParseUint(fields[0], 10, 64)
		if perr != nil {
			continue
		}
		kb := v * 1024
		switch key {
		case "MemTotal":
			memTotal = kb
			haveTotal = true
		case "MemAvailable":
			memAvailable = kb
			haveAvail = true
		}
	}
	if !haveTotal {
		return 0, 0, fmt.Errorf("sysmetrics: MemTotal missing in /proc/meminfo")
	}
	if !haveAvail {
		return 0, 0, fmt.Errorf("sysmetrics: MemAvailable missing in /proc/meminfo")
	}
	if memAvailable > memTotal {
		memAvailable = memTotal
	}
	return memTotal, memAvailable, nil
}

// parseDiskstats sums sectors read/written for whole disks only.
func parseDiskstats(data string) (diskStat, error) {
	var out diskStat
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		// major minor name ... sectors_read(idx5) ... sectors_written(idx9)
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if !isWholeDisk(name) {
			continue
		}
		sectorsRead, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			continue
		}
		sectorsWrite, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		out.sectorsRead += sectorsRead
		out.sectorsWrite += sectorsWrite
	}
	return out, nil
}

// isWholeDisk reports whether name is a whole-disk device (not a partition).
func isWholeDisk(name string) bool {
	return wholeDiskRE.MatchString(name)
}

// parseLoadavg parses the first three fields of /proc/loadavg.
func parseLoadavg(data string) (l1, l5, l15 float64, err error) {
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("sysmetrics: unexpected /proc/loadavg: %q", data)
	}
	l1, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	l5, err = strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	l15, err = strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	return l1, l5, l15, nil
}

// parseUptime parses the first field of /proc/uptime.
func parseUptime(data string) (float64, error) {
	fields := strings.Fields(data)
	if len(fields) < 1 {
		return 0, fmt.Errorf("sysmetrics: empty /proc/uptime")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// cpuPercentFromDelta computes CPU busy percent from two counter samples.
func cpuPercentFromDelta(prev, cur cpuStat) float64 {
	totalDelta := cur.total - prev.total
	idleDelta := cur.idle - prev.idle
	if totalDelta == 0 {
		return 0
	}
	pct := (1 - float64(idleDelta)/float64(totalDelta)) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}
