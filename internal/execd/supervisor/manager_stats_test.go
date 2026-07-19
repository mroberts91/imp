// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/execd/cgroups"
	"github.com/mroberts91/imp/internal/metrics"
)

// ProcStats is a pure read over the cgroup snapshots: exercised here
// against a fake cgroup tree, without booting the full supervisor stack.
func TestProcStats(t *testing.T) {
	root := t.TempDir()
	if err := cgroups.SetupFakeRoot(root); err != nil {
		t.Fatalf("SetupFakeRoot: %v", err)
	}
	cg, err := cgroups.NewManager(root)
	if err != nil {
		t.Fatalf("cgroups.NewManager: %v", err)
	}

	writeCgroupFiles := func(dir string, mem, cpu, pids string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"memory.current": mem,
			"cpu.stat":       "usage_usec " + cpu + "\n",
			"pids.current":   pids,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	webDir := writeCgroupFiles(filepath.Join(root, "web-0-abc"), "1048576", "250000", "3")
	// No pids.current for the timer proc: best-effort zero, not an error.
	timerDir := filepath.Join(root, "backup-123")
	writeCgroupFiles(timerDir, "2048", "99", "0")
	if err := os.Remove(filepath.Join(timerDir, "pids.current")); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		cgroups: cg,
		clock:   clock.NewFake(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)),
		cgroupSnaps: map[string]metrics.ProcCgroup{
			"Proc/web-0-abc":  {Proc: "web-0-abc", Daemon: "web", Path: webDir},
			"Proc/backup-123": {Proc: "backup-123", Timer: "backup", Path: timerDir},
			"Proc/gone":       {Proc: "gone", Daemon: "web", Path: filepath.Join(root, "does-not-exist")},
		},
	}

	stats := m.ProcStats()
	if len(stats) != 2 {
		t.Fatalf("got %d stats, want 2 (vanished cgroup skipped): %+v", len(stats), stats)
	}
	// Sorted by proc name: backup-123 first.
	if stats[0].Proc != "backup-123" || stats[1].Proc != "web-0-abc" {
		t.Fatalf("order = %s, %s; want backup-123, web-0-abc", stats[0].Proc, stats[1].Proc)
	}
	if o := stats[0].Owner; o.Kind != v1alpha1.KindTimer || o.Name != "backup" {
		t.Errorf("timer proc owner = %+v, want Timer/backup", o)
	}
	if stats[0].PidsCurrent != 0 {
		t.Errorf("missing pids.current should read 0, got %d", stats[0].PidsCurrent)
	}
	w := stats[1]
	if o := w.Owner; o.Kind != v1alpha1.KindDaemon || o.Name != "web" {
		t.Errorf("daemon proc owner = %+v, want Daemon/web", o)
	}
	if w.CPUUsageUsec != 250000 || w.MemoryCurrentBytes != 1048576 || w.PidsCurrent != 3 {
		t.Errorf("stats = cpu %d mem %d pids %d, want 250000/1048576/3",
			w.CPUUsageUsec, w.MemoryCurrentBytes, w.PidsCurrent)
	}
	if w.SampledAt.IsZero() {
		t.Error("SampledAt not stamped")
	}
}
