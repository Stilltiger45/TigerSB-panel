// Package metrics samples the server's resources from /proc every few minutes.
//
// Rates (CPU, network) are averages over the time since the previous sample.
// A gap, a reboot or a counter reset leaves them empty instead of inventing a
// value. CPU time taken by the hypervisor (steal) is kept out of the CPU
// figure and shown on its own, an idea from web-panel-proxy (MIT).
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

// Interval is how often the monitor timer takes a sample.
const Interval = 5 * time.Minute

// Keep is how long samples are stored.
const Keep = 7 * 24 * time.Hour

// maxGap is the longest pause after which rates are still averaged.
const maxGap = 3 * Interval

// Sample is one reading of the server's resources.
type Sample struct {
	Time      int64     `json:"time"`
	CPU       *float64  `json:"cpu"`   // percent of the server's own CPU time
	Steal     *float64  `json:"steal"` // percent taken by the hypervisor
	Cores     int       `json:"cores"`
	Load      []float64 `json:"load"`
	MemUsed   uint64    `json:"mem_used"`
	MemTotal  uint64    `json:"mem_total"`
	SwapUsed  uint64    `json:"swap_used"`
	SwapTotal uint64    `json:"swap_total"`
	DiskUsed  uint64    `json:"disk_used"`
	DiskTotal uint64    `json:"disk_total"`
	RxRate    *float64  `json:"rx_rate"` // bytes per second, all interfaces but loopback
	TxRate    *float64  `json:"tx_rate"`
	Uptime    int64     `json:"uptime"`

	Raw Raw `json:"raw"`
}

// Raw holds the counters the next sample needs for its averages.
type Raw struct {
	BootID   string `json:"boot_id"`
	CPUAll   uint64 `json:"cpu_all"`
	CPUIdle  uint64 `json:"cpu_idle"`
	CPUSteal uint64 `json:"cpu_steal"`
	Rx       uint64 `json:"rx"`
	Tx       uint64 `json:"tx"`
}

// Source reads the counters. Tests point it at a fake /proc.
type Source struct {
	Proc string // usually /proc
	Disk string // the file system to measure, usually /
}

// Host reads the real server.
var Host = Source{Proc: "/proc", Disk: "/"}

func (src Source) read(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(src.Proc, name))
	return string(b), err
}

// cpu returns all, idle (idle + iowait) and steal ticks, and the core count.
func (src Source) cpu() (all, idle, steal uint64, cores int, err error) {
	text, err := src.read("stat")
	if err != nil {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] != "cpu" {
			if strings.HasPrefix(f[0], "cpu") {
				cores++
			}
			continue
		}
		// user nice system idle iowait irq softirq steal; guest time is already in user.
		var v [8]uint64
		for i := 0; i < len(v) && i+1 < len(f); i++ {
			v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
		}
		for _, x := range v {
			all += x
		}
		idle, steal = v[3]+v[4], v[7]
	}
	if all == 0 {
		err = fmt.Errorf("в %s/stat нет строки cpu", src.Proc)
	}
	return
}

func (src Source) memory(s *Sample) error {
	text, err := src.read("meminfo")
	if err != nil {
		return err
	}
	m := map[string]uint64{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			v, _ := strconv.ParseUint(f[1], 10, 64)
			m[strings.TrimSuffix(f[0], ":")] = v * 1024
		}
	}
	avail, ok := m["MemAvailable"]
	if !ok {
		avail = m["MemFree"]
	}
	s.MemTotal = m["MemTotal"]
	if s.MemTotal == 0 {
		return fmt.Errorf("в %s/meminfo нет MemTotal", src.Proc)
	}
	s.MemUsed = s.MemTotal - min(avail, s.MemTotal)
	s.SwapTotal = m["SwapTotal"]
	s.SwapUsed = s.SwapTotal - min(m["SwapFree"], s.SwapTotal)
	return nil
}

func (src Source) network() (rx, tx uint64) {
	text, _ := src.read("net/dev")
	for _, line := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return
}

// Read takes a sample. prev is the previous sample or nil; without one the CPU
// is measured over one second right here.
func (src Source) Read(prev *Sample, now time.Time) (*Sample, error) {
	s := &Sample{Time: now.Unix()}
	all, idle, steal, cores, err := src.cpu()
	if err != nil {
		return nil, err
	}
	s.Cores = max(cores, 1)
	boot, _ := src.read("sys/kernel/random/boot_id")
	s.Raw = Raw{BootID: strings.TrimSpace(boot), CPUAll: all, CPUIdle: idle, CPUSteal: steal}
	s.Raw.Rx, s.Raw.Tx = src.network()
	if err := src.memory(s); err != nil {
		return nil, err
	}
	if text, err := src.read("loadavg"); err == nil {
		f := strings.Fields(text)
		for _, x := range f[:min(3, len(f))] {
			v, _ := strconv.ParseFloat(x, 64)
			s.Load = append(s.Load, v)
		}
	}
	if text, err := src.read("uptime"); err == nil {
		if f := strings.Fields(text); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			s.Uptime = int64(v)
		}
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(src.Disk, &fs); err == nil {
		s.DiskTotal = fs.Blocks * uint64(fs.Bsize)
		s.DiskUsed = s.DiskTotal - fs.Bfree*uint64(fs.Bsize)
	}

	usable := prev != nil && prev.Raw.BootID == s.Raw.BootID && s.Raw.BootID != "" &&
		s.Time > prev.Time && time.Duration(s.Time-prev.Time)*time.Second <= maxGap
	if usable {
		s.CPU, s.Steal = cpuPercent(prev.Raw, s.Raw)
		dt := float64(s.Time - prev.Time)
		if s.Raw.Rx >= prev.Raw.Rx && s.Raw.Tx >= prev.Raw.Tx {
			rx, tx := float64(s.Raw.Rx-prev.Raw.Rx)/dt, float64(s.Raw.Tx-prev.Raw.Tx)/dt
			s.RxRate, s.TxRate = &rx, &tx
		}
	} else if prev == nil {
		time.Sleep(time.Second)
		if all2, idle2, steal2, _, err := src.cpu(); err == nil {
			s.CPU, s.Steal = cpuPercent(s.Raw, Raw{CPUAll: all2, CPUIdle: idle2, CPUSteal: steal2})
		}
	}
	return s, nil
}

func cpuPercent(a, b Raw) (cpu, steal *float64) {
	if b.CPUAll <= a.CPUAll || b.CPUIdle < a.CPUIdle || b.CPUSteal < a.CPUSteal {
		return nil, nil
	}
	elapsed := float64(b.CPUAll - a.CPUAll)
	st := float64(b.CPUSteal - a.CPUSteal)
	own := elapsed - st
	st = clamp(100 * st / elapsed)
	steal = &st
	if own > 0 {
		c := clamp(100 * (1 - float64(b.CPUIdle-a.CPUIdle)/own))
		cpu = &c
	}
	return
}

func clamp(v float64) float64 { return max(0, min(100, v)) }

// Collect takes a sample, stores it and drops samples older than Keep.
func Collect(ctx context.Context, st *store.Store, src Source, now time.Time) (*Sample, error) {
	prev, err := Last(ctx, st)
	if err != nil {
		return nil, err
	}
	s, err := src.Read(prev, now)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if err := st.AddMetric(ctx, now, data); err != nil {
		return nil, err
	}
	return s, st.PruneMetrics(ctx, now.Add(-Keep))
}

// Last returns the newest stored sample, or nil.
func Last(ctx context.Context, st *store.Store) (*Sample, error) {
	data, err := st.LastMetric(ctx)
	if err != nil || data == nil {
		return nil, err
	}
	var s Sample
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, nil // a broken row only costs one average
	}
	return &s, nil
}

// History returns stored samples since the given time, oldest first.
func History(ctx context.Context, st *store.Store, since time.Time) ([]Sample, error) {
	rows, err := st.Metrics(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(rows))
	for _, r := range rows {
		var s Sample
		if json.Unmarshal(r, &s) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}
