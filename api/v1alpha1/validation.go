// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Name and label legality rules are logical forks of Kubernetes'
// staging/src/k8s.io/apimachinery/pkg/util/validation/validation.go
// (Copyright The Kubernetes Authors, Apache-2.0).

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/robfig/cron/v3"
)

const (
	maxNameLength       = 253
	maxLabelPartLength  = 63
	dns1123SubdomainFmt = "a lowercase RFC 1123 subdomain: alphanumeric segments separated by '.', '-' allowed inside segments"
)

var (
	dns1123SubdomainRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	// labelPartRegexp covers both the name part of a qualified key and a
	// non-empty label value: alphanumeric at both ends, [-_.] allowed inside.
	labelPartRegexp = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

func IsValidKind(kind string) bool {
	_, ok := allowedKids[kind]
	return ok
}

// knownSignals are the signal names accepted for stopSignal, without the SIG
// prefix. Hand-rolled rather than consulting
// x/sys/unix so this package builds on every platform impctl does.
var knownSignals = map[string]bool{
	"ABRT": true, "ALRM": true, "BUS": true, "CHLD": true, "CONT": true,
	"FPE": true, "HUP": true, "ILL": true, "INT": true, "IO": true,
	"KILL": true, "PIPE": true, "PROF": true, "PWR": true, "QUIT": true,
	"SEGV": true, "STOP": true, "SYS": true, "TERM": true, "TRAP": true,
	"TSTP": true, "TTIN": true, "TTOU": true, "URG": true, "USR1": true,
	"USR2": true, "VTALRM": true, "WINCH": true, "XCPU": true, "XFSZ": true,
}

// IsKnownSignal reports whether name (with or without the SIG prefix) is a
// recognized signal name.
func IsKnownSignal(name string) bool {
	return knownSignals[strings.TrimPrefix(name, "SIG")]
}

func ValidateDaemon(d *Daemon) ErrorList {
	var errs ErrorList
	errs = append(errs, validateObjectMeta(&d.Metadata, NewPath("metadata"))...)

	specPath := NewPath("spec")
	switch {
	case d.Spec.Replicas == nil:
		errs = append(errs, requiredErr(specPath.Child("replicas"), ""))
	case *d.Spec.Replicas < 0:
		errs = append(errs, invalidErr(specPath.Child("replicas"), *d.Spec.Replicas, "must be greater than or equal to 0"))
	}

	stratPath := specPath.Child("updateStrategy")
	switch d.Spec.UpdateStrategy.Type {
	case UpdateStrategyRecreate:
		if d.Spec.UpdateStrategy.RollingUpdate != nil {
			errs = append(errs, invalidErr(
				stratPath.Child("rollingUpdate"),
				d.Spec.UpdateStrategy.RollingUpdate,
				"may only be set when type is RollingUpdate",
			))
		}
	case UpdateStrategyRollingUpdate:
		if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil && *ru.Partition < 0 {
			errs = append(errs, invalidErr(
				stratPath.Child("rollingUpdate").Child("partition"),
				*ru.Partition,
				"must be greater than or equal to 0",
			))
		}
	default:
		errs = append(errs, notSupportedErr(
			stratPath.Child("type"),
			d.Spec.UpdateStrategy.Type,
			[]string{string(UpdateStrategyRecreate), string(UpdateStrategyRollingUpdate)},
		))
	}

	tplMetaPath := specPath.Child("template").Child("metadata")
	errs = append(errs, validateLabels(d.Spec.Template.Metadata.Labels, tplMetaPath.Child("labels"))...)
	errs = append(errs, validateAnnotations(d.Spec.Template.Metadata.Annotations, tplMetaPath.Child("annotations"))...)
	errs = append(errs, validateProcTemplateSpec(&d.Spec.Template.Spec, specPath.Child("template").Child("spec"))...)
	return errs
}

func ValidateProc(p *Proc) ErrorList {
	var errs ErrorList
	errs = append(errs, validateObjectMeta(&p.Metadata, NewPath("metadata"))...)
	errs = append(errs, validateProcTemplateSpec(&p.Spec, NewPath("spec"))...)
	return errs
}

func ValidateTimer(t *Timer) ErrorList {
	var errs ErrorList
	errs = append(errs, validateObjectMeta(&t.Metadata, NewPath("metadata"))...)

	specPath := NewPath("spec")
	if t.Spec.Schedule == "" {
		errs = append(errs, requiredErr(specPath.Child("schedule"), ""))
	} else if _, err := cron.ParseStandard(t.Spec.Schedule); err != nil {
		errs = append(errs, invalidErr(specPath.Child("schedule"), t.Spec.Schedule, err.Error()))
	}

	switch t.Spec.ConcurrencyPolicy {
	case ConcurrencyForbid, ConcurrencyAllow, ConcurrencyReplace:
	default:
		errs = append(errs, notSupportedErr(specPath.Child("concurrencyPolicy"), t.Spec.ConcurrencyPolicy,
			[]string{string(ConcurrencyForbid), string(ConcurrencyAllow), string(ConcurrencyReplace)}))
	}

	if v := t.Spec.StartingDeadlineSeconds; v != nil && *v < 0 {
		errs = append(errs, invalidErr(specPath.Child("startingDeadlineSeconds"), *v, "must be greater than or equal to 0"))
	}
	if v := t.Spec.SuccessfulHistoryLimit; v != nil && *v < 0 {
		errs = append(errs, invalidErr(specPath.Child("successfulHistoryLimit"), *v, "must be greater than or equal to 0"))
	}
	if v := t.Spec.FailedHistoryLimit; v != nil && *v < 0 {
		errs = append(errs, invalidErr(specPath.Child("failedHistoryLimit"), *v, "must be greater than or equal to 0"))
	}

	tplMetaPath := specPath.Child("template").Child("metadata")
	errs = append(errs, validateLabels(t.Spec.Template.Metadata.Labels, tplMetaPath.Child("labels"))...)
	errs = append(errs, validateAnnotations(t.Spec.Template.Metadata.Annotations, tplMetaPath.Child("annotations"))...)
	tplSpecPath := specPath.Child("template").Child("spec")
	errs = append(errs, validateProcTemplateSpec(&t.Spec.Template.Spec, tplSpecPath)...)

	// A Timer run must terminate: Always is the daemon posture and would
	// respawn the process forever.
	switch t.Spec.Template.Spec.RestartPolicy {
	case RestartPolicyNever, RestartPolicyOnFailure:
	default:
		errs = append(errs, notSupportedErr(tplSpecPath.Child("restartPolicy"), t.Spec.Template.Spec.RestartPolicy,
			[]string{string(RestartPolicyNever), string(RestartPolicyOnFailure)}))
	}
	return errs
}

func ValidateEvent(e *Event) ErrorList {
	var errs ErrorList
	errs = append(errs, validateObjectMeta(&e.Metadata, NewPath("metadata"))...)
	if e.Regarding.Kind == "" {
		errs = append(errs, requiredErr(NewPath("regarding").Child("kind"), ""))
	}
	if e.Regarding.Name == "" {
		errs = append(errs, requiredErr(NewPath("regarding").Child("name"), ""))
	}
	if e.Type != EventTypeNormal && e.Type != EventTypeWarning {
		errs = append(errs, notSupportedErr(NewPath("type"), e.Type,
			[]string{string(EventTypeNormal), string(EventTypeWarning)}))
	}
	if e.Reason == "" {
		errs = append(errs, requiredErr(NewPath("reason"), ""))
	}
	return errs
}

func validateObjectMeta(m *ObjectMeta, p *Path) ErrorList {
	var errs ErrorList
	switch {
	case m.Name == "":
		errs = append(errs, requiredErr(p.Child("name"), ""))
	case len(m.Name) > maxNameLength:
		errs = append(errs, invalidErr(p.Child("name"), m.Name, fmt.Sprintf("must be no more than %d characters", maxNameLength)))
	case !dns1123SubdomainRegexp.MatchString(m.Name):
		errs = append(errs, invalidErr(p.Child("name"), m.Name, "must be "+dns1123SubdomainFmt))
	}
	errs = append(errs, validateLabels(m.Labels, p.Child("labels"))...)
	errs = append(errs, validateAnnotations(m.Annotations, p.Child("annotations"))...)
	return errs
}

func validateProcTemplateSpec(s *ProcTemplateSpec, p *Path) ErrorList {
	var errs ErrorList
	switch {
	case len(s.Command) == 0:
		errs = append(errs, requiredErr(p.Child("command"), ""))
	case s.Command[0] == "":
		errs = append(errs, invalidErr(p.Child("command").Index(0), s.Command[0], "executable must not be empty"))
	}

	for i, env := range s.Env {
		if env.Name == "" {
			errs = append(errs, requiredErr(p.Child("env").Index(i).Child("name"), ""))
		}
	}

	switch s.RestartPolicy {
	case RestartPolicyAlways, RestartPolicyOnFailure, RestartPolicyNever:
	default:
		errs = append(errs, notSupportedErr(p.Child("restartPolicy"), s.RestartPolicy,
			[]string{string(RestartPolicyAlways), string(RestartPolicyOnFailure), string(RestartPolicyNever)}))
	}

	if !IsKnownSignal(s.StopSignal) {
		errs = append(errs, invalidErr(p.Child("stopSignal"), s.StopSignal, "must be a known signal name, e.g. TERM, INT, HUP"))
	}

	if s.TerminationGracePeriodSeconds != nil && *s.TerminationGracePeriodSeconds < 0 {
		errs = append(errs, invalidErr(p.Child("terminationGracePeriodSeconds"), *s.TerminationGracePeriodSeconds, "must be greater than or equal to 0"))
	}

	errs = append(errs, validateResourceRequirements(s.Resources, p.Child("resources"))...)
	if s.LivenessProbe != nil {
		errs = append(errs, validateProbe(s.LivenessProbe, p.Child("livenessProbe"), true)...)
	}
	if s.ReadinessProbe != nil {
		errs = append(errs, validateProbe(s.ReadinessProbe, p.Child("readinessProbe"), false)...)
	}
	if s.LogRetention != nil {
		errs = append(errs, validateLogRetention(s.LogRetention, p.Child("logRetention"))...)
	}
	return errs
}

func validateLogRetention(lr *LogRetention, p *Path) ErrorList {
	var errs ErrorList
	if v := lr.MaxSizeMB; v != nil && *v < 1 {
		errs = append(errs, invalidErr(p.Child("maxSizeMB"), *v, "must be greater than or equal to 1"))
	}
	if v := lr.MaxBackups; v != nil && *v < 0 {
		errs = append(errs, invalidErr(p.Child("maxBackups"), *v, "must be greater than or equal to 0"))
	}
	if v := lr.MaxAgeDays; v != nil && *v < 0 {
		errs = append(errs, invalidErr(p.Child("maxAgeDays"), *v, "must be greater than or equal to 0"))
	}
	return errs
}

func validateResourceRequirements(r ResourceRequirements, p *Path) ErrorList {
	if r.Limits.Empty() {
		return nil
	}
	var errs ErrorList
	limPath := p.Child("limits")
	if r.Limits.Memory != "" {
		if _, err := ParseMemoryBytes(r.Limits.Memory); err != nil {
			errs = append(errs, invalidErr(limPath.Child("memory"), r.Limits.Memory, err.Error()))
		}
	}
	if r.Limits.CPUWeight != nil {
		w := *r.Limits.CPUWeight
		if w < 1 || w > 10000 {
			errs = append(errs, invalidErr(limPath.Child("cpuWeight"), w, "must be between 1 and 10000"))
		}
	}
	if r.Limits.Pids != nil && *r.Limits.Pids < 1 {
		errs = append(errs, invalidErr(limPath.Child("pids"), *r.Limits.Pids, "must be greater than or equal to 1"))
	}
	return errs
}

func validateProbe(probe *Probe, p *Path, liveness bool) ErrorList {
	var errs ErrorList
	handlers := 0
	if probe.Exec != nil {
		handlers++
	}
	if probe.HTTPGet != nil {
		handlers++
	}
	if probe.TCPSocket != nil {
		handlers++
	}
	switch handlers {
	case 0:
		errs = append(errs, requiredErr(p, "exactly one of exec, httpGet, or tcpSocket is required"))
	case 1:
		// ok
	default:
		errs = append(errs, invalidErr(p, handlers, "exactly one of exec, httpGet, or tcpSocket is required"))
	}

	if probe.Exec != nil {
		execPath := p.Child("exec")
		switch {
		case len(probe.Exec.Command) == 0:
			errs = append(errs, requiredErr(execPath.Child("command"), ""))
		case probe.Exec.Command[0] == "":
			errs = append(errs, invalidErr(execPath.Child("command").Index(0), probe.Exec.Command[0], "executable must not be empty"))
		}
	}
	if probe.HTTPGet != nil {
		errs = append(errs, validatePort(probe.HTTPGet.Port, p.Child("httpGet").Child("port"))...)
		switch probe.HTTPGet.Scheme {
		case "", URISchemeHTTP, URISchemeHTTPS:
		default:
			errs = append(errs, notSupportedErr(p.Child("httpGet").Child("scheme"), probe.HTTPGet.Scheme,
				[]string{string(URISchemeHTTP), string(URISchemeHTTPS)}))
		}
		for i, h := range probe.HTTPGet.HTTPHeaders {
			if h.Name == "" {
				errs = append(errs, requiredErr(p.Child("httpGet").Child("httpHeaders").Index(i).Child("name"), ""))
			}
		}
	}
	if probe.TCPSocket != nil {
		errs = append(errs, validatePort(probe.TCPSocket.Port, p.Child("tcpSocket").Child("port"))...)
	}

	if probe.InitialDelaySeconds < 0 {
		errs = append(errs, invalidErr(p.Child("initialDelaySeconds"), probe.InitialDelaySeconds, "must be greater than or equal to 0"))
	}
	if probe.TimeoutSeconds < 1 {
		errs = append(errs, invalidErr(p.Child("timeoutSeconds"), probe.TimeoutSeconds, "must be greater than or equal to 1"))
	}
	if probe.PeriodSeconds < 1 {
		errs = append(errs, invalidErr(p.Child("periodSeconds"), probe.PeriodSeconds, "must be greater than or equal to 1"))
	}
	if probe.SuccessThreshold < 1 {
		errs = append(errs, invalidErr(p.Child("successThreshold"), probe.SuccessThreshold, "must be greater than or equal to 1"))
	}
	if liveness && probe.SuccessThreshold != 1 {
		errs = append(errs, invalidErr(p.Child("successThreshold"), probe.SuccessThreshold, "must be 1 for liveness probes"))
	}
	if probe.FailureThreshold < 1 {
		errs = append(errs, invalidErr(p.Child("failureThreshold"), probe.FailureThreshold, "must be greater than or equal to 1"))
	}
	return errs
}

func validatePort(port int32, p *Path) ErrorList {
	if port < 1 || port > 65535 {
		return ErrorList{invalidErr(p, port, "must be between 1 and 65535")}
	}
	return nil
}

func validateLabels(labels map[string]string, p *Path) ErrorList {
	var errs ErrorList
	for k, v := range labels {
		if msg := validateQualifiedKey(k); msg != "" {
			errs = append(errs, invalidErr(p.Key(k), k, msg))
		}
		if msg := validateLabelValue(v); msg != "" {
			errs = append(errs, invalidErr(p.Key(k), v, msg))
		}
	}
	return errs
}

// validateAnnotations checks key legality only. Annotation values are
// unrestricted.
func validateAnnotations(annotations map[string]string, p *Path) ErrorList {
	var errs ErrorList
	for k := range annotations {
		if msg := validateQualifiedKey(k); msg != "" {
			errs = append(errs, invalidErr(p.Key(k), k, msg))
		}
	}
	return errs
}

// validateQualifiedKey checks a label or annotation key: an optional DNS-1123
// subdomain prefix and '/', followed by a name part of at most 63 characters,
// alphanumeric at both ends with [-_.] allowed inside.
func validateQualifiedKey(key string) string {
	name := key
	if prefix, rest, found := strings.Cut(key, "/"); found {
		switch {
		case prefix == "":
			return "prefix part must not be empty"
		case len(prefix) > maxNameLength || !dns1123SubdomainRegexp.MatchString(prefix):
			return "prefix part must be " + dns1123SubdomainFmt
		}
		name = rest
	}
	switch {
	case name == "":
		return "name part must not be empty"
	case len(name) > maxLabelPartLength:
		return fmt.Sprintf("name part must be no more than %d characters", maxLabelPartLength)
	case !labelPartRegexp.MatchString(name):
		return "name part must be alphanumeric at both ends with '-', '_' or '.' inside"
	}
	return ""
}

func validateLabelValue(value string) string {
	switch {
	case value == "":
		return ""
	case len(value) > maxLabelPartLength:
		return fmt.Sprintf("label value must be no more than %d characters", maxLabelPartLength)
	case !labelPartRegexp.MatchString(value):
		return "label value must be empty or alphanumeric at both ends with '-', '_' or '.' inside"
	}
	return ""
}
