// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package childsetup

// The identity/capability transition (M7) is specified as a pure plan —
// sandboxPlan returns the exact ordered syscall-level steps, pinned by
// unprivileged unit tests — and executed step by step in apply_linux.go.
// The sequencing is transcribed from capabilities(7) and prctl(2), and
// cross-checked against systemd's exec_invoke ordering and runc's
// libcontainer/capabilities behavior (references only; no code is forked).

import (
	"slices"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// step is one syscall-level action in the sandbox/identity transition,
// comparable in tests. This file is pure and builds on every platform.
type step struct {
	op string
	// arg is the op's operand: a capability name (capbset_drop,
	// ambient_raise), []int groups (setgroups), an int id (setgid, setuid),
	// or []string capability names (capset_inheritable).
	arg any
}

const (
	opCapBsetDrop       = "capbset_drop"
	opKeepcapsOn        = "keepcaps_on"
	opSetgroups         = "setgroups"
	opSetgid            = "setgid"
	opSetuid            = "setuid"
	opCapsetInheritable = "capset_inheritable"
	opAmbientRaise      = "ambient_raise"
	opKeepcapsOff       = "keepcaps_off"
)

// capNumbers maps the API's lowercase capability names (validated against
// v1alpha1.IsKnownCapability) to kernel capability numbers. Hand-rolled so
// this file builds everywhere; pinned against the API table (plan_test.go)
// and against the unix.CAP_* constants (apply_linux_test.go).
var capNumbers = map[string]int{
	"chown": 0, "dac_override": 1, "dac_read_search": 2, "fowner": 3,
	"fsetid": 4, "kill": 5, "setgid": 6, "setuid": 7, "setpcap": 8,
	"linux_immutable": 9, "net_bind_service": 10, "net_broadcast": 11,
	"net_admin": 12, "net_raw": 13, "ipc_lock": 14, "ipc_owner": 15,
	"sys_module": 16, "sys_rawio": 17, "sys_chroot": 18, "sys_ptrace": 19,
	"sys_pacct": 20, "sys_admin": 21, "sys_boot": 22, "sys_nice": 23,
	"sys_resource": 24, "sys_time": 25, "sys_tty_config": 26, "mknod": 27,
	"lease": 28, "audit_write": 29, "audit_control": 30, "setfcap": 31,
	"mac_override": 32, "mac_admin": 33, "syslog": 34, "wake_alarm": 35,
	"block_suspend": 36, "audit_read": 37, "perfmon": 38, "bpf": 39,
	"checkpoint_restore": 40,
}

// sandboxPlan returns the ordered steps for the bounding-drop and
// identity/capability transition. Without Capabilities the plan degenerates
// to the M6 identity sequence exactly (groups → gid → uid).
//
// With Ambient set, the sequence is (capabilities(7), prctl(2)):
//
//  1. keepcaps on — the permitted set survives the coming setuid (the
//     effective set is still cleared; nothing later needs it).
//  2. setgroups → setgid → setuid (the M6 order; groups need privilege).
//  3. capset: permitted and effective pass through unchanged, inheritable =
//     the ambient set. Legal without CAP_SETPCAP because capset only
//     requires new I ⊆ old I ∪ P, and ambient ⊆ P is guaranteed by
//     keep-caps.
//  4. ambient raise per cap — requires the cap in P ∩ I, established above.
//     Raised only after the setuid: a UID change clears the ambient set.
//  5. keepcaps off — hygiene; exec would clear it anyway.
//
// No capability leaks: at execve of a no-file-caps binary by a non-root
// uid, P' = E' = ambient — the caps keep-caps retained in P that were not
// raised to ambient vanish (P' gains nothing from I ∩ fI when fI = ∅).
//
// Bounding drops go first: PR_CAPBSET_DROP needs CAP_SETPCAP in the
// effective set, which the setuid clears. Every table capability not
// listed is dropped, in capability-number order.
func sandboxPlan(id *identity, caps *v1alpha1.Capabilities) []step {
	var steps []step
	if caps != nil && len(caps.Bounding) > 0 {
		for _, name := range capNamesByNumber() {
			if !slices.Contains(caps.Bounding, name) {
				steps = append(steps, step{opCapBsetDrop, name})
			}
		}
	}
	ambient := caps != nil && len(caps.Ambient) > 0
	if ambient {
		steps = append(steps, step{opKeepcapsOn, nil})
	}
	if id != nil {
		steps = append(steps, step{opSetgroups, id.groups})
		if id.gid >= 0 {
			steps = append(steps, step{opSetgid, id.gid})
		}
		if id.uid >= 0 {
			steps = append(steps, step{opSetuid, id.uid})
		}
	}
	if ambient {
		steps = append(steps, step{opCapsetInheritable, slices.Clone(caps.Ambient)})
		for _, name := range caps.Ambient {
			steps = append(steps, step{opAmbientRaise, name})
		}
		steps = append(steps, step{opKeepcapsOff, nil})
	}
	return steps
}

// capNamesByNumber returns every table capability name in kernel
// capability-number order.
func capNamesByNumber() []string {
	names := make([]string, len(capNumbers))
	for name, n := range capNumbers {
		names[n] = name
	}
	return names
}
