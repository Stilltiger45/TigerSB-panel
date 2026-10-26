package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stilltiger45/TigerSB-panel/internal/store"
)

func fakeProc(t *testing.T, dir, stat, netdev, boot string) Source {
	t.Helper()
	files := map[string]string{
		"stat":                      stat,
		"meminfo":                   "MemTotal: 2000 kB\nMemFree: 100 kB\nMemAvailable: 500 kB\nSwapTotal: 1000 kB\nSwapFree: 1000 kB\n",
		"loadavg":                   "0.50 0.25 0.10 1/100 1234\n",
		"uptime":                    "3600.55 7000.00\n",
		"net/dev":                   netdev,
		"sys/kernel/random/boot_id": boot + "\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Source{Proc: dir, Disk: dir}
}

const netHead = "Inter-|   Receive  |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets\n"

func TestAveragesBetweenSamples(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1_800_000_000, 0)
	// user nice system idle iowait irq softirq steal
	src := fakeProc(t, dir, "cpu  100 0 100 700 0 0 0 100\ncpu0 1\ncpu1 1\n",
		netHead+"    lo: 999999 0 0 0 0 0 0 0 999999 0\n  eth0: 1000 0 0 0 0 0 0 0 2000 0\n", "boot-a")
	first, err := src.Read(&Sample{Time: t0.Add(-time.Hour).Unix(), Raw: Raw{BootID: "boot-a"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if first.CPU != nil || first.RxRate != nil {
		t.Fatalf("an hour-old sample must not be averaged: %+v", first)
	}
	if first.Cores != 2 || first.MemTotal != 2000*1024 || first.MemUsed != 1500*1024 || first.Uptime != 3600 || len(first.Load) != 3 {
		t.Fatalf("first: %+v", first)
	}

	// 1000 more ticks: 100 steal, 600 idle, 300 busy; 3000 bytes in and 6000 out over 300 s.
	fakeProc(t, dir, "cpu  250 0 250 1300 0 0 0 200\n",
		netHead+"  eth0: 4000 0 0 0 0 0 0 0 8000 0\n", "boot-a")
	next, err := src.Read(first, t0.Add(Interval))
	if err != nil {
		t.Fatal(err)
	}
	if next.CPU == nil || int(*next.CPU+0.5) != 33 || next.Steal == nil || int(*next.Steal+0.5) != 10 {
		t.Fatalf("cpu %v steal %v, want 33 and 10", ptrv(next.CPU), ptrv(next.Steal))
	}
	if *next.RxRate != 10 || *next.TxRate != 20 {
		t.Fatalf("rates %v %v", *next.RxRate, *next.TxRate)
	}

	// After a reboot the counters start over: no averages.
	fakeProc(t, dir, "cpu  1 0 1 10 0 0 0 0\n", netHead+"  eth0: 5 0 0 0 0 0 0 0 5 0\n", "boot-b")
	if again, _ := src.Read(next, t0.Add(2*Interval)); again.CPU != nil || again.RxRate != nil {
		t.Fatalf("averaged across a reboot: %+v", again)
	}
}

func ptrv(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestCollectStoresAndPrunes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	src := fakeProc(t, t.TempDir(), "cpu  1 0 1 10 0 0 0 0\n", netHead, "b")
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		if _, err := Collect(ctx, st, src, t0.Add(time.Duration(i)*Interval)); err != nil {
			t.Fatal(err)
		}
	}
	if h, _ := History(ctx, st, t0); len(h) != 3 {
		t.Fatalf("history: %d", len(h))
	}
	if _, err := Collect(ctx, st, src, t0.Add(Keep+Interval+time.Second)); err != nil {
		t.Fatal(err)
	}
	if h, _ := History(ctx, st, time.Unix(0, 0)); len(h) != 2 {
		t.Fatalf("after prune: %d samples", len(h))
	}
}
