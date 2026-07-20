// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Name and label legality rules are logical forks of Kubernetes'
// staging/src/k8s.io/apimachinery/pkg/util/validation/validation.go
// (Copyright The Kubernetes Authors, Apache-2.0).

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxNameLength       = 253
	maxLabelPartLength  = 63
	dns1123SubdomainFmt = "a lowercase RFC 1123 subdomain: alphanumeric segments separated by '.', '-' allowed inside segments"

	// Config size caps (M8-f): k8s ConfigMap parity.
	maxConfigFiles     = 64
	maxConfigTotalSize = 1 << 20 // 1 MiB of total content per Config.
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

// knownRlimits are the lowercase RLIMIT_* resource names accepted in
// spec.rlimits. Hand-rolled table (no x/sys import) for the same reason as
// knownSignals; the shim owns the name → RLIMIT_* constant mapping.
var knownRlimits = map[string]bool{
	"as": true, "core": true, "cpu": true, "data": true, "fsize": true,
	"locks": true, "memlock": true, "msgqueue": true, "nice": true,
	"nofile": true, "nproc": true, "rss": true, "rtprio": true,
	"sigpending": true, "stack": true,
}

// IsKnownRlimit reports whether name is a recognized rlimit resource name.
func IsKnownRlimit(name string) bool {
	return knownRlimits[name]
}

// knownCapabilities are the lowercase Linux capability names (without the
// CAP_ prefix, M7-a) accepted in spec.capabilities — all 41 capabilities,
// CAP_CHOWN (0) through CAP_CHECKPOINT_RESTORE (40). Hand-rolled table (no
// x/sys import) for the same reason as knownSignals; the childsetup shim
// owns the name → CAP_* constant mapping and pins it against this list.
var knownCapabilities = map[string]bool{
	"chown": true, "dac_override": true, "dac_read_search": true,
	"fowner": true, "fsetid": true, "kill": true, "setgid": true,
	"setuid": true, "setpcap": true, "linux_immutable": true,
	"net_bind_service": true, "net_broadcast": true, "net_admin": true,
	"net_raw": true, "ipc_lock": true, "ipc_owner": true,
	"sys_module": true, "sys_rawio": true, "sys_chroot": true,
	"sys_ptrace": true, "sys_pacct": true, "sys_admin": true,
	"sys_boot": true, "sys_nice": true, "sys_resource": true,
	"sys_time": true, "sys_tty_config": true, "mknod": true,
	"lease": true, "audit_write": true, "audit_control": true,
	"setfcap": true, "mac_override": true, "mac_admin": true,
	"syslog": true, "wake_alarm": true, "block_suspend": true,
	"audit_read": true, "perfmon": true, "bpf": true,
	"checkpoint_restore": true,
}

// IsKnownCapability reports whether name is a recognized capability name.
func IsKnownCapability(name string) bool {
	return knownCapabilities[name]
}

// KnownCapabilityNames returns the accepted capability names, sorted. The
// childsetup shim pins its CAP_* mapping against this list in tests.
func KnownCapabilityNames() []string {
	names := make([]string, 0, len(knownCapabilities))
	for name := range knownCapabilities {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// KnownRlimitNames returns the accepted rlimit resource names, sorted. The
// childsetup shim pins its RLIMIT_* mapping against this list in tests.
func KnownRlimitNames() []string {
	names := make([]string, 0, len(knownRlimits))
	for name := range knownRlimits {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var umaskRegexp = regexp.MustCompile(`^[0-7]{3,4}$`)

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
		if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil {
			if ru.Partition != nil && *ru.Partition < 0 {
				errs = append(errs, invalidErr(
					stratPath.Child("rollingUpdate").Child("partition"),
					*ru.Partition,
					"must be greater than or equal to 0",
				))
			}
			if ru.MaxUnavailable != nil && *ru.MaxUnavailable < 1 {
				errs = append(errs, invalidErr(
					stratPath.Child("rollingUpdate").Child("maxUnavailable"),
					*ru.MaxUnavailable,
					"must be greater than or equal to 1",
				))
			}
		}
	default:
		errs = append(errs, notSupportedErr(
			stratPath.Child("type"),
			d.Spec.UpdateStrategy.Type,
			[]string{string(UpdateStrategyRecreate), string(UpdateStrategyRollingUpdate)},
		))
	}

	if d.Spec.MinReadySeconds < 0 {
		errs = append(errs, invalidErr(specPath.Child("minReadySeconds"), d.Spec.MinReadySeconds, "must be greater than or equal to 0"))
	}
	if v := d.Spec.ProgressDeadlineSeconds; v != nil && *v <= d.Spec.MinReadySeconds {
		// The Deployment rule: a deadline shorter than the availability
		// delay could never be met.
		errs = append(errs, invalidErr(specPath.Child("progressDeadlineSeconds"), *v, "must be greater than minReadySeconds"))
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
	} else {
		// Validate the zone on its own field first; only then re-parse the
		// schedule in that zone (the same composition the controller does).
		zoneOK := true
		if t.Spec.TimeZone != nil {
			if _, err := time.LoadLocation(*t.Spec.TimeZone); err != nil {
				errs = append(errs, invalidErr(specPath.Child("timeZone"), *t.Spec.TimeZone, err.Error()))
				zoneOK = false
			}
		}
		if zoneOK {
			if _, err := ParseTimerSchedule(&t.Spec); err != nil {
				errs = append(errs, invalidErr(specPath.Child("schedule"), t.Spec.Schedule, err.Error()))
			}
		}
	}
	if v := t.Spec.JitterSeconds; v != nil && *v < 0 {
		errs = append(errs, invalidErr(specPath.Child("jitterSeconds"), *v, "must be greater than or equal to 0"))
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

func ValidateConfig(c *Config) ErrorList {
	var errs ErrorList
	errs = append(errs, validateObjectMeta(&c.Metadata, NewPath("metadata"))...)

	specPath := NewPath("spec")
	dataPath := specPath.Child("data")
	binaryPath := specPath.Child("binaryData")

	// Files and total bytes are counted across data + binaryData (M9-d); the
	// aggregate errors stay on spec.data, the primary field.
	fileCount := len(c.Spec.Data) + len(c.Spec.BinaryData)
	switch {
	case fileCount == 0:
		errs = append(errs, requiredErr(dataPath, "at least one file is required (data or binaryData)"))
	case fileCount > maxConfigFiles:
		errs = append(errs, invalidErr(dataPath, fileCount,
			fmt.Sprintf("must not contain more than %d files across data and binaryData", maxConfigFiles)))
	}
	total := 0
	for name, content := range c.Spec.Data {
		if msg := configFilenameMsg(name); msg != "" {
			errs = append(errs, invalidErr(dataPath.Key(name), name, msg))
		}
		total += len(content)
	}
	for name, content := range c.Spec.BinaryData {
		if msg := configFilenameMsg(name); msg != "" {
			errs = append(errs, invalidErr(binaryPath.Key(name), name, msg))
		}
		if _, dup := c.Spec.Data[name]; dup {
			errs = append(errs, invalidErr(binaryPath.Key(name), name, "filename must not also appear in data"))
		}
		total += len(content)
	}
	if total > maxConfigTotalSize {
		errs = append(errs, invalidErr(dataPath, total,
			fmt.Sprintf("total content size must be no more than %d bytes (1 MiB)", maxConfigTotalSize)))
	}
	if c.Spec.Mode != nil {
		if msg := configModeMsg(*c.Spec.Mode); msg != "" {
			errs = append(errs, invalidErr(specPath.Child("mode"), *c.Spec.Mode, msg))
		}
	}
	for name, mode := range c.Spec.Modes {
		mp := specPath.Child("modes").Key(name)
		_, inData := c.Spec.Data[name]
		_, inBinary := c.Spec.BinaryData[name]
		if !inData && !inBinary {
			errs = append(errs, invalidErr(mp, name, "must name a file present in data or binaryData"))
		}
		if msg := configModeMsg(mode); msg != "" {
			errs = append(errs, invalidErr(mp, mode, msg))
		}
	}
	return errs
}

// configModeMsg validates a Config file mode: 3-4 octal digits naming plain
// permission bits (000-777). Special bits (setuid/setgid/sticky — a non-zero
// leading digit) are rejected because they are meaningless for a regular file
// and would be silently dropped when the mode becomes an os.FileMode at
// materialization; a mode that grants no owner read is rejected because the
// process — which owns the file after the chown — could then never read it.
func configModeMsg(mode string) string {
	if !umaskRegexp.MatchString(mode) {
		return `must be 3-4 octal digits, e.g. "0644"`
	}
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil { // unreachable after the regex, but do not trust it blindly
		return "must be a valid octal file mode"
	}
	switch {
	case m > 0o777:
		return "must not set special bits (setuid/setgid/sticky); only permission bits 000-777 are supported for config files"
	case m&0o400 == 0:
		return `must grant owner read (e.g. "0644" or "0600") so the process can read the file`
	}
	return ""
}

// configFilenameMsg returns "" if name is a legal Config filename — a single
// path component — else the reason it is not. Rejecting '/', "." and ".."
// keeps materialization inside the per-proc config dir (no path traversal).
func configFilenameMsg(name string) string {
	switch {
	case name == "":
		return "filename must not be empty"
	case len(name) > maxNameLength:
		return fmt.Sprintf("filename must be no more than %d characters", maxNameLength)
	case name == "." || name == "..":
		return `filename must not be "." or ".."`
	case strings.ContainsRune(name, '/'):
		return "filename must be a single path component (no '/')"
	}
	return ""
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
	if s.StartupProbe != nil {
		// Like liveness, a startup probe's failure restarts the process, so
		// successThreshold must be 1 (kubelet rule).
		errs = append(errs, validateProbe(s.StartupProbe, p.Child("startupProbe"), true)...)
	}
	errs = append(errs, validateRlimits(s.Rlimits, p.Child("rlimits"))...)
	if s.Nice != nil && (*s.Nice < -20 || *s.Nice > 19) {
		errs = append(errs, invalidErr(p.Child("nice"), *s.Nice, "must be between -20 and 19"))
	}
	if s.OOMScoreAdjust != nil && (*s.OOMScoreAdjust < -1000 || *s.OOMScoreAdjust > 1000) {
		errs = append(errs, invalidErr(p.Child("oomScoreAdjust"), *s.OOMScoreAdjust, "must be between -1000 and 1000"))
	}
	if s.Umask != nil && !umaskRegexp.MatchString(*s.Umask) {
		errs = append(errs, invalidErr(p.Child("umask"), *s.Umask, `must be 3-4 octal digits, e.g. "0022"`))
	}
	if s.LogRetention != nil {
		errs = append(errs, validateLogRetention(s.LogRetention, p.Child("logRetention"))...)
	}
	if s.Capabilities != nil {
		errs = append(errs, validateCapabilities(s.Capabilities, p.Child("capabilities"))...)
	}
	errs = append(errs, validateConfigRefs(s.Configs, p.Child("configs"))...)
	return errs
}

// validateConfigRefs checks each Config reference is a legal object name with a
// clean absolute path (when set), and that neither names nor paths collide.
// Existence is deliberately NOT checked here: manifest-dir creation order is
// free, and the runtime missing-config story is M8-h's (the DaemonController
// holds until the Config appears).
func validateConfigRefs(refs []ConfigRef, p *Path) ErrorList {
	var errs ErrorList
	seenName := map[string]bool{}
	seenPath := map[string]bool{}
	for i, ref := range refs {
		switch {
		case ref.Name == "":
			errs = append(errs, requiredErr(p.Index(i), ""))
		case len(ref.Name) > maxNameLength || !dns1123SubdomainRegexp.MatchString(ref.Name):
			errs = append(errs, invalidErr(p.Index(i), ref.Name, "must be "+dns1123SubdomainFmt))
		case seenName[ref.Name]:
			errs = append(errs, invalidErr(p.Index(i), ref.Name, "duplicate config reference"))
		default:
			seenName[ref.Name] = true
		}
		if ref.Path != nil {
			errs = append(errs, validateConfigRefPath(*ref.Path, p.Index(i).Child("path"), seenPath)...)
		}
	}
	return errs
}

// validateConfigRefPath checks a per-reference destination path (M9-b): it must
// be absolute, already clean, not the root, and unique among the template's
// refs (two refs sharing a directory would collide on materialization).
func validateConfigRefPath(path string, p *Path, seen map[string]bool) ErrorList {
	switch {
	case !filepath.IsAbs(path):
		return ErrorList{invalidErr(p, path, "must be an absolute path")}
	case filepath.Clean(path) != path:
		return ErrorList{invalidErr(p, path, "must be a clean path (no '.', '..', or trailing/repeated '/')")}
	case path == "/":
		return ErrorList{invalidErr(p, path, "must not be the root directory")}
	case seen[path]:
		return ErrorList{invalidErr(p, path, "duplicate reference path — two refs would materialize into the same directory")}
	}
	seen[path] = true
	return nil
}

func validateCapabilities(c *Capabilities, p *Path) ErrorList {
	var errs ErrorList
	if len(c.Bounding) == 0 && len(c.Ambient) == 0 {
		return ErrorList{requiredErr(p, "at least one of bounding or ambient is required")}
	}
	// M7-g: an explicit empty bounding list ("drop every capability") is not
	// expressible — omitempty makes it indistinguishable from absent after a
	// storage round-trip, and silently flipping "drop all" to "don't touch"
	// is not acceptable. Run as a user with noNewPrivileges instead.
	if c.Bounding != nil && len(c.Bounding) == 0 {
		errs = append(errs, invalidErr(p.Child("bounding"), c.Bounding,
			"must name at least one capability to keep; dropping every capability is not supported — run as an unprivileged user with noNewPrivileges instead"))
	}
	errs = append(errs, validateCapabilityList(c.Bounding, p.Child("bounding"))...)
	errs = append(errs, validateCapabilityList(c.Ambient, p.Child("ambient"))...)
	// M7-h: an ambient cap outside the bounding set is a self-contradiction
	// ("keep only these" vs "also grant that one"). systemd resolves it by
	// silently unioning ambient into the bounding set; imp rejects it.
	if len(c.Bounding) > 0 && len(c.Ambient) > 0 {
		for i, name := range c.Ambient {
			if IsKnownCapability(name) && !slices.Contains(c.Bounding, name) {
				errs = append(errs, invalidErr(p.Child("ambient").Index(i), name,
					"must also be listed in bounding (an ambient capability outside the bounding set is a contradiction)"))
			}
		}
	}
	return errs
}

func validateCapabilityList(names []string, p *Path) ErrorList {
	var errs ErrorList
	seen := map[string]bool{}
	for i, name := range names {
		switch {
		case name == "":
			errs = append(errs, requiredErr(p.Index(i), ""))
		case !IsKnownCapability(name):
			errs = append(errs, invalidErr(p.Index(i), name,
				"must be a lowercase capability name without the CAP_ prefix, e.g. net_bind_service"))
		case seen[name]:
			errs = append(errs, invalidErr(p.Index(i), name, "duplicate capability"))
		default:
			seen[name] = true
		}
	}
	return errs
}

func validateRlimits(rlimits []Rlimit, p *Path) ErrorList {
	var errs ErrorList
	seen := map[string]bool{}
	for i, r := range rlimits {
		rp := p.Index(i)
		switch {
		case r.Resource == "":
			errs = append(errs, requiredErr(rp.Child("resource"), ""))
		case !IsKnownRlimit(r.Resource):
			errs = append(errs, invalidErr(rp.Child("resource"), r.Resource, "must be a lowercase rlimit resource name, e.g. nofile, core, nproc"))
		case seen[r.Resource]:
			errs = append(errs, invalidErr(rp.Child("resource"), r.Resource, "duplicate resource"))
		default:
			seen[r.Resource] = true
		}
		if r.Soft == nil && r.Hard == nil {
			errs = append(errs, requiredErr(rp, "at least one of soft or hard is required"))
			continue
		}
		if r.Soft != nil && *r.Soft < RlimitInfinity {
			errs = append(errs, invalidErr(rp.Child("soft"), *r.Soft, "must be greater than or equal to -1 (-1 means unlimited)"))
		}
		if r.Hard != nil && *r.Hard < RlimitInfinity {
			errs = append(errs, invalidErr(rp.Child("hard"), *r.Hard, "must be greater than or equal to -1 (-1 means unlimited)"))
		}
		if r.Soft != nil && r.Hard != nil {
			soft, hard := *r.Soft, *r.Hard
			// -1 is infinity: an unlimited soft over a finite hard is invalid.
			if soft == RlimitInfinity && hard != RlimitInfinity {
				errs = append(errs, invalidErr(rp.Child("soft"), soft, "soft may not be unlimited when hard is finite"))
			} else if soft != RlimitInfinity && hard != RlimitInfinity && soft > hard {
				errs = append(errs, invalidErr(rp.Child("soft"), soft, "must be less than or equal to hard"))
			}
		}
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
	if r.Limits.CPU != "" {
		if _, err := ParseCPUMax(r.Limits.CPU); err != nil {
			errs = append(errs, invalidErr(limPath.Child("cpu"), r.Limits.CPU, err.Error()))
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
	// Probe-level grace (M9-l): liveness/startup only — a readiness failure
	// never kills, so a grace there is meaningless (k8s parity). The liveness
	// flag is true for both liveness and startup probes.
	if v := probe.TerminationGracePeriodSeconds; v != nil {
		switch {
		case !liveness:
			errs = append(errs, invalidErr(p.Child("terminationGracePeriodSeconds"), *v,
				"may not be set on readiness probes (a readiness failure does not kill the process)"))
		case *v < 1:
			errs = append(errs, invalidErr(p.Child("terminationGracePeriodSeconds"), *v, "must be greater than or equal to 1"))
		}
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
