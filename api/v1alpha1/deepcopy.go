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
	out.Template = *in.Template.DeepCopy()
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
	return &out
}
