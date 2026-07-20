// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"maps"
	"slices"
)

// DeepCopy returns a copy sharing no memory with the original.
func (in *Daemon) DeepCopy() *Daemon {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	out.Spec = *in.Spec.DeepCopy()
	out.Status = *in.Status.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Proc) DeepCopy() *Proc {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	out.Spec = *in.Spec.DeepCopy()
	out.Status = *in.Status.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Event) DeepCopy() *Event {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ObjectMeta) DeepCopy() *ObjectMeta {
	if in == nil {
		return nil
	}
	out := *in
	out.Labels = maps.Clone(in.Labels)
	out.Annotations = maps.Clone(in.Annotations)
	out.OwnerReferences = slices.Clone(in.OwnerReferences)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *DaemonSpec) DeepCopy() *DaemonSpec {
	if in == nil {
		return nil
	}
	out := *in
	if in.Replicas != nil {
		out.Replicas = new(*in.Replicas)
	}
	if in.UpdateStrategy.RollingUpdate != nil {
		out.UpdateStrategy.RollingUpdate = in.UpdateStrategy.RollingUpdate.DeepCopy()
	}
	if in.ProgressDeadlineSeconds != nil {
		out.ProgressDeadlineSeconds = new(*in.ProgressDeadlineSeconds)
	}
	out.Template = *in.Template.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *RollingUpdateDaemonStrategy) DeepCopy() *RollingUpdateDaemonStrategy {
	if in == nil {
		return nil
	}
	out := *in
	if in.Partition != nil {
		out.Partition = new(*in.Partition)
	}
	if in.MaxUnavailable != nil {
		out.MaxUnavailable = new(*in.MaxUnavailable)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ProcTemplate) DeepCopy() *ProcTemplate {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata.Labels = maps.Clone(in.Metadata.Labels)
	out.Metadata.Annotations = maps.Clone(in.Metadata.Annotations)
	out.Spec = *in.Spec.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original. Because
// ProcSpec is an alias of ProcTemplateSpec, this covers both.
func (in *ProcTemplateSpec) DeepCopy() *ProcTemplateSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.Command = slices.Clone(in.Command)
	out.Env = slices.Clone(in.Env)
	if in.TerminationGracePeriodSeconds != nil {
		out.TerminationGracePeriodSeconds = new(*in.TerminationGracePeriodSeconds)
	}
	out.Resources = *in.Resources.DeepCopy()
	if in.LivenessProbe != nil {
		out.LivenessProbe = in.LivenessProbe.DeepCopy()
	}
	if in.ReadinessProbe != nil {
		out.ReadinessProbe = in.ReadinessProbe.DeepCopy()
	}
	if in.StartupProbe != nil {
		out.StartupProbe = in.StartupProbe.DeepCopy()
	}
	if in.Rlimits != nil {
		out.Rlimits = make([]Rlimit, len(in.Rlimits))
		for i := range in.Rlimits {
			out.Rlimits[i] = *in.Rlimits[i].DeepCopy()
		}
	}
	if in.Nice != nil {
		out.Nice = new(*in.Nice)
	}
	if in.OOMScoreAdjust != nil {
		out.OOMScoreAdjust = new(*in.OOMScoreAdjust)
	}
	if in.Umask != nil {
		out.Umask = new(*in.Umask)
	}
	if in.LogRetention != nil {
		out.LogRetention = in.LogRetention.DeepCopy()
	}
	if in.NoNewPrivileges != nil {
		out.NoNewPrivileges = new(*in.NoNewPrivileges)
	}
	if in.Capabilities != nil {
		out.Capabilities = in.Capabilities.DeepCopy()
	}
	if in.PrivateTmp != nil {
		out.PrivateTmp = new(*in.PrivateTmp)
	}
	if in.Configs != nil {
		out.Configs = make([]ConfigRef, len(in.Configs))
		for i := range in.Configs {
			out.Configs[i] = in.Configs[i]
			if in.Configs[i].Path != nil {
				out.Configs[i].Path = new(*in.Configs[i].Path)
			}
		}
	}
	if in.Filesystem != nil {
		out.Filesystem = in.Filesystem.DeepCopy()
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Config) DeepCopy() *Config {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	out.Spec = *in.Spec.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ConfigSpec) DeepCopy() *ConfigSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.Data = maps.Clone(in.Data)
	if in.BinaryData != nil {
		out.BinaryData = make(map[string][]byte, len(in.BinaryData))
		for k, v := range in.BinaryData {
			out.BinaryData[k] = slices.Clone(v)
		}
	}
	if in.Mode != nil {
		out.Mode = new(*in.Mode)
	}
	out.Modes = maps.Clone(in.Modes)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Capabilities) DeepCopy() *Capabilities {
	if in == nil {
		return nil
	}
	out := *in
	out.Bounding = slices.Clone(in.Bounding)
	out.Ambient = slices.Clone(in.Ambient)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *FilesystemPolicy) DeepCopy() *FilesystemPolicy {
	if in == nil {
		return nil
	}
	out := *in
	if in.ReadOnlyRoot != nil {
		out.ReadOnlyRoot = new(*in.ReadOnlyRoot)
	}
	if in.ProtectHome != nil {
		out.ProtectHome = new(*in.ProtectHome)
	}
	out.ReadWritePaths = slices.Clone(in.ReadWritePaths)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Rlimit) DeepCopy() *Rlimit {
	if in == nil {
		return nil
	}
	out := *in
	if in.Soft != nil {
		out.Soft = new(*in.Soft)
	}
	if in.Hard != nil {
		out.Hard = new(*in.Hard)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *LogRetention) DeepCopy() *LogRetention {
	if in == nil {
		return nil
	}
	out := *in
	if in.MaxSizeMB != nil {
		out.MaxSizeMB = new(*in.MaxSizeMB)
	}
	if in.MaxBackups != nil {
		out.MaxBackups = new(*in.MaxBackups)
	}
	if in.MaxAgeDays != nil {
		out.MaxAgeDays = new(*in.MaxAgeDays)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Timer) DeepCopy() *Timer {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	out.Spec = *in.Spec.DeepCopy()
	out.Status = *in.Status.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *TimerSpec) DeepCopy() *TimerSpec {
	if in == nil {
		return nil
	}
	out := *in
	if in.TimeZone != nil {
		out.TimeZone = new(*in.TimeZone)
	}
	if in.JitterSeconds != nil {
		out.JitterSeconds = new(*in.JitterSeconds)
	}
	if in.CatchUp != nil {
		out.CatchUp = new(*in.CatchUp)
	}
	if in.Suspend != nil {
		out.Suspend = new(*in.Suspend)
	}
	if in.StartingDeadlineSeconds != nil {
		out.StartingDeadlineSeconds = new(*in.StartingDeadlineSeconds)
	}
	if in.SuccessfulHistoryLimit != nil {
		out.SuccessfulHistoryLimit = new(*in.SuccessfulHistoryLimit)
	}
	if in.FailedHistoryLimit != nil {
		out.FailedHistoryLimit = new(*in.FailedHistoryLimit)
	}
	out.Template = *in.Template.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *TimerStatus) DeepCopy() *TimerStatus {
	if in == nil {
		return nil
	}
	out := *in
	out.Conditions = slices.Clone(in.Conditions)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Notifier) DeepCopy() *Notifier {
	if in == nil {
		return nil
	}
	out := *in
	out.Metadata = *in.Metadata.DeepCopy()
	out.Spec = *in.Spec.DeepCopy()
	out.Status = *in.Status.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *NotifierSpec) DeepCopy() *NotifierSpec {
	if in == nil {
		return nil
	}
	out := *in
	out.Template = *in.Template.DeepCopy()
	if in.CooldownSeconds != nil {
		out.CooldownSeconds = new(*in.CooldownSeconds)
	}
	if in.MinRestarts != nil {
		out.MinRestarts = new(*in.MinRestarts)
	}
	if in.HistoryLimit != nil {
		out.HistoryLimit = new(*in.HistoryLimit)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *NotifierStatus) DeepCopy() *NotifierStatus {
	if in == nil {
		return nil
	}
	out := *in
	out.Conditions = slices.Clone(in.Conditions)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ResourceRequirements) DeepCopy() *ResourceRequirements {
	if in == nil {
		return nil
	}
	out := *in
	out.Limits = *in.Limits.DeepCopy()
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ResourceLimits) DeepCopy() *ResourceLimits {
	if in == nil {
		return nil
	}
	out := *in
	if in.CPUWeight != nil {
		out.CPUWeight = new(*in.CPUWeight)
	}
	if in.Pids != nil {
		out.Pids = new(*in.Pids)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *Probe) DeepCopy() *Probe {
	if in == nil {
		return nil
	}
	out := *in
	if in.Exec != nil {
		out.Exec = &ExecAction{Command: slices.Clone(in.Exec.Command)}
	}
	if in.HTTPGet != nil {
		out.HTTPGet = &HTTPGetAction{
			Path:        in.HTTPGet.Path,
			Port:        in.HTTPGet.Port,
			Host:        in.HTTPGet.Host,
			Scheme:      in.HTTPGet.Scheme,
			HTTPHeaders: slices.Clone(in.HTTPGet.HTTPHeaders),
		}
	}
	if in.TCPSocket != nil {
		out.TCPSocket = new(*in.TCPSocket)
	}
	if in.TerminationGracePeriodSeconds != nil {
		out.TerminationGracePeriodSeconds = new(*in.TerminationGracePeriodSeconds)
	}
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *DaemonStatus) DeepCopy() *DaemonStatus {
	if in == nil {
		return nil
	}
	out := *in
	out.Conditions = slices.Clone(in.Conditions)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ProcStatus) DeepCopy() *ProcStatus {
	if in == nil {
		return nil
	}
	out := *in
	out.State = *in.State.DeepCopy()
	out.Conditions = slices.Clone(in.Conditions)
	return &out
}

// DeepCopy returns a copy sharing no memory with the original.
func (in *ProcState) DeepCopy() *ProcState {
	if in == nil {
		return nil
	}
	out := *in
	if in.Waiting != nil {
		out.Waiting = new(*in.Waiting)
	}
	if in.Running != nil {
		out.Running = new(*in.Running)
	}
	if in.Terminated != nil {
		out.Terminated = new(*in.Terminated)
		if in.Terminated.ExitCode != nil {
			out.Terminated.ExitCode = new(*in.Terminated.ExitCode)
		}
	}
	if in.LastTerminated != nil {
		out.LastTerminated = new(*in.LastTerminated)
		if in.LastTerminated.ExitCode != nil {
			out.LastTerminated.ExitCode = new(*in.LastTerminated.ExitCode)
		}
	}
	return &out
}
