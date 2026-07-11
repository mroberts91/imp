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

	if d.Spec.UpdateStrategy.Type != UpdateStrategyRecreate {
		errs = append(errs, notSupportedErr(
			specPath.Child("updateStrategy").Child("type"),
			d.Spec.UpdateStrategy.Type,
			[]string{string(UpdateStrategyRecreate)},
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
	return errs
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
