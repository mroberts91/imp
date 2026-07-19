// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

// Package childsetup is the child-setup shim (doc 10 M6-a): every Proc spawn
// goes impd → /proc/self/exe (this package, triggered by an env var checked
// before anything else in main) → execve(target). Running setup inside the
// child, between fork and exec, is what lets rlimits be raised while still
// privileged and the identity drop be ordered after them — the systemd
// sequence — with no post-spawn race. The pattern (env-triggered init mode
// re-execing self) is runc/libcontainer's; no code is forked from it.
//
// The shim never returns control to impd's main: it either execve()s the
// target (same pid, start ticks, process group, fds, and cgroup — nothing
// upstream can tell the shim was there) or exits with ExitCode and a
// one-line "imp child-setup: …" message on stderr, which is already wired
// to the Proc's log. A fast exit 126 therefore lands in CrashLoopBackOff
// with the reason one `impctl logs` away.
package childsetup

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// EnvName carries the JSON payload from the supervisor into the shim. It is
// scrubbed from the environment before the target is exec'd.
const EnvName = "_IMP_CHILD_SETUP"

// ExitCode is the shim's exit status when setup fails — the shell's
// "cannot execute" convention (M6-h).
const ExitCode = 126

// Payload is everything the shim needs. The supervisor resolves the
// executable parent-side (PATH semantics identical to the pre-shim
// exec.Command behavior); the shim does no lookups beyond identity.
//
// There is deliberately no version field: /proc/self/exe opens the running
// impd's inode even if the binary on disk was replaced mid-upgrade, so the
// shim is always the same build as the supervisor that spawned it.
type Payload struct {
	// Exe is the path to execve: absolute when Command[0] was PATH-resolved,
	// as-written when it contains a separator (the kernel resolves it after
	// the chdir into workingDir, preserving relative-workdir semantics).
	Exe  string   `json:"exe"`
	Argv []string `json:"argv"`
	// User/Group are names, resolved (again) in the shim for the actual
	// drop; the supervisor pre-checks them parent-side so a typo fails at
	// start with a clean error instead of an exit-126 loop.
	User           string            `json:"user,omitempty"`
	Group          string            `json:"group,omitempty"`
	Rlimits        []v1alpha1.Rlimit `json:"rlimits,omitempty"`
	Nice           *int32            `json:"nice,omitempty"`
	OOMScoreAdjust *int32            `json:"oomScoreAdjust,omitempty"`
	Umask          *string           `json:"umask,omitempty"`
}

// EnvEntry renders the payload as the "NAME=json" environment entry the
// supervisor appends to the child env.
func (p *Payload) EnvEntry() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshaling child-setup payload: %w", err)
	}
	return EnvName + "=" + string(b), nil
}

// MaybeRun is called first thing in impd's main, before flag parsing. When
// the payload env var is absent it returns immediately (normal impd start).
// When present, this process is a freshly spawned Proc child: apply the
// setup and execve the target — this call never returns. Any failure prints
// to stderr (the Proc's log) and exits ExitCode.
func MaybeRun() {
	raw := os.Getenv(EnvName)
	if raw == "" {
		return
	}
	_ = os.Unsetenv(EnvName) // the target must not inherit the payload
	var p Payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		fail(fmt.Errorf("parsing payload: %w", err))
	}
	fail(apply(&p)) // apply returns only on error
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "imp child-setup: %v\n", err)
	os.Exit(ExitCode)
}

// identity is the resolved uid/gid/supplementary set for the drop.
// uid/gid of -1 mean "leave unchanged".
type identity struct {
	uid, gid int
	groups   []int
}

// resolveIdentity turns user/group names into the identity to apply.
// Semantics (M6-f): a user brings their supplementary groups (initgroups
// behavior, as systemd and login(1) do); an explicit group overrides the
// primary gid but not the supplementary list. Group-only keeps the uid and
// clears supplementary groups — the pre-M6 SysProcAttr.Credential behavior.
func resolveIdentity(userName, groupName string) (*identity, error) {
	id := &identity{uid: -1, gid: -1, groups: []int{}}
	if userName != "" {
		u, err := user.Lookup(userName)
		if err != nil {
			return nil, fmt.Errorf("looking up user %q: %w", userName, err)
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return nil, fmt.Errorf("parsing uid for %q: %w", userName, err)
		}
		gid, err := strconv.Atoi(u.Gid)
		if err != nil {
			return nil, fmt.Errorf("parsing gid for %q: %w", userName, err)
		}
		id.uid, id.gid = uid, gid
		groupIDs, err := u.GroupIds()
		if err != nil {
			return nil, fmt.Errorf("listing groups for %q: %w", userName, err)
		}
		for _, g := range groupIDs {
			n, err := strconv.Atoi(g)
			if err != nil {
				return nil, fmt.Errorf("parsing group id %q for %q: %w", g, userName, err)
			}
			id.groups = append(id.groups, n)
		}
	}
	if groupName != "" {
		g, err := user.LookupGroup(groupName)
		if err != nil {
			return nil, fmt.Errorf("looking up group %q: %w", groupName, err)
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return nil, fmt.Errorf("parsing gid for group %q: %w", groupName, err)
		}
		id.gid = gid
	}
	return id, nil
}

// CheckIdentity verifies that the user/group names resolve, without applying
// anything. The supervisor calls it parent-side so a misspelled user is a
// clean start error, not a crash loop.
func CheckIdentity(userName, groupName string) error {
	if userName == "" && groupName == "" {
		return nil
	}
	_, err := resolveIdentity(userName, groupName)
	return err
}
