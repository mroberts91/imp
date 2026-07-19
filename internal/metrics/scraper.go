// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/mroberts91/imp/internal/clock"
	"github.com/mroberts91/imp/internal/execd/cgroups"
)

// ProcCgroup is one running Proc's cgroup path for resource scrapes.
type ProcCgroup struct {
	Proc, Daemon, Path string
}

// ProcLister returns current Proc cgroup membership for the scraper.
type ProcLister interface {
	ListProcCgroups() []ProcCgroup
}

const defaultScrapeInterval = 15 * time.Second

// Scraper periodically reads cgroup Stats into memory/cpu metrics.
type Scraper struct {
	reg      *Registry
	cg       *cgroups.Manager
	lister   ProcLister
	clk      clock.Clock
	interval time.Duration
	log      *slog.Logger
}

// NewScraper builds a cgroup stats scraper. interval <= 0 uses 15s.
func NewScraper(reg *Registry, cg *cgroups.Manager, lister ProcLister, clk clock.Clock, interval time.Duration) *Scraper {
	if clk == nil {
		clk = clock.Real{}
	}
	if interval <= 0 {
		interval = defaultScrapeInterval
	}
	return &Scraper{
		reg:      reg,
		cg:       cg,
		lister:   lister,
		clk:      clk,
		interval: interval,
		log:      slog.With("component", "metrics"),
	}
}

// Run scrapes until ctx is cancelled.
func (s *Scraper) Run(ctx context.Context) {
	if s == nil || s.reg == nil || s.cg == nil || s.lister == nil {
		return
	}
	ticker := s.clk.NewTicker(s.interval)
	defer ticker.Stop()
	s.scrape()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			s.scrape()
		}
	}
}

func (s *Scraper) scrape() {
	for _, p := range s.lister.ListProcCgroups() {
		if p.Path == "" || p.Proc == "" {
			continue
		}
		st, err := s.cg.Stats(p.Path)
		if err != nil {
			s.log.Debug("cgroup stats scrape failed", "proc", p.Proc, "cgroup", p.Path, "error", err)
			continue
		}
		s.reg.SetProcMemory(p.Proc, p.Daemon, st.MemoryCurrent)
		s.reg.ObserveProcCPU(p.Proc, p.Daemon, float64(st.CPUUsageUsec)/1e6)
	}
}
