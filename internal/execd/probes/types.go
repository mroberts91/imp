// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package probes runs per-Proc liveness/readiness checks. Workers report
// results; the supervisor acts (Ready condition / restart). Threshold and
// initial-delay semantics are a logical fork of
// pkg/kubelet/prober/worker.go (Copyright The Kubernetes Authors, Apache-2.0;
// see LICENSES/kubernetes/).
package probes

import (
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// ProbeType distinguishes liveness vs readiness workers.
type ProbeType string

const (
	ProbeStartup   ProbeType = "Startup"
	ProbeLiveness  ProbeType = "Liveness"
	ProbeReadiness ProbeType = "Readiness"
)

// Result is the outcome of one probe execution.
type Result string

const (
	ResultSuccess Result = "Success"
	ResultFailure Result = "Failure"
	ResultUnknown Result = "Unknown"
)

// ResultEvent is emitted when the *effective* probe result flips after
// success/failure thresholds (kubelet worker SM).
type ResultEvent struct {
	ProcKey   string
	ProbeType ProbeType
	Result    Result
	Message   string
	At        time.Time
}

// ProbeSpec is the configured probe (already defaulted by the API).
type ProbeSpec = v1alpha1.Probe
