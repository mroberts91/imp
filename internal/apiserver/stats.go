// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"net/http"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// StatsProvider is what the api-server needs from the supervisor to serve
// `impctl top`: point-in-time resource observations for running Procs. It
// is implemented by execd's manager (the same cgroup membership the metrics
// scraper reads). Observations are not objects — they never touch the store
// (doc 08 M5-e).
type StatsProvider interface {
	ProcStats() []v1alpha1.ProcStat
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	if s.stats == nil {
		writeJSON(w, http.StatusNotImplemented, &v1alpha1.APIError{
			Reason:  v1alpha1.ErrorReasonInternal,
			Message: "proc stats are not available in this build",
		})
		return
	}
	items := s.stats.ProcStats()
	if items == nil {
		items = []v1alpha1.ProcStat{}
	}
	writeJSON(w, http.StatusOK, v1alpha1.StatsList{Items: items})
}
