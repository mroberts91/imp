// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/execd/childsetup"
)

// shimExe is what every Proc spawn execs (M6-a always-shim). /proc/self/exe
// pins the running impd's inode, so the shim is the same build as this
// supervisor even if the binary on disk was replaced mid-upgrade.
const shimExe = "/proc/self/exe"

// buildCmd constructs the Cmd for p without starting it. stdout/stderr must
// already be open (log writers). Environment is explicit — never inherits
// impd's env (supervisor information-leak discipline).
//
// The spawn goes through the childsetup shim: impd → /proc/self/exe →
// execve(target). execve preserves pid, start ticks, process group, fds,
// and cgroup membership, so everything downstream (D1 identity, pidfd,
// probes, log capture) is oblivious to it. cmd.Dir still applies — os/exec
// chdirs in the forked child before exec, and the shim inherits that.
func buildCmd(p *v1alpha1.Proc, stdout, stderr io.Writer) (*exec.Cmd, error) {
	if len(p.Spec.Command) == 0 {
		return nil, fmt.Errorf("proc %s: empty command", p.Metadata.Name)
	}
	// Resolve the executable exactly as exec.Command did pre-shim: a name
	// without a path separator goes through impd's own PATH; a name with
	// one is left as written for the kernel to resolve after the chdir
	// (workingDir-relative commands keep working).
	exe := p.Spec.Command[0]
	if !strings.Contains(exe, "/") {
		resolved, err := exec.LookPath(exe)
		if err != nil {
			return nil, fmt.Errorf("proc %s: %w", p.Metadata.Name, err)
		}
		exe = resolved
	}
	// Pre-check identity parent-side so a misspelled user is a clean start
	// error, not an exit-126 crash loop; the shim re-resolves for the drop.
	if err := childsetup.CheckIdentity(p.Spec.User, p.Spec.Group); err != nil {
		return nil, fmt.Errorf("proc %s: %w", p.Metadata.Name, err)
	}
	payload := childsetup.Payload{
		Exe:             exe,
		Argv:            p.Spec.Command,
		User:            p.Spec.User,
		Group:           p.Spec.Group,
		Rlimits:         p.Spec.Rlimits,
		Nice:            p.Spec.Nice,
		OOMScoreAdjust:  p.Spec.OOMScoreAdjust,
		Umask:           p.Spec.Umask,
		NoNewPrivileges: p.Spec.NoNewPrivileges,
		Capabilities:    p.Spec.Capabilities,
		PrivateTmp:      p.Spec.PrivateTmp,
	}
	entry, err := payload.EnvEntry()
	if err != nil {
		return nil, fmt.Errorf("proc %s: %w", p.Metadata.Name, err)
	}

	cmd := exec.Command(shimExe)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = p.Spec.WorkingDir
	cmd.Env = append(buildEnv(p), entry)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func buildEnv(p *v1alpha1.Proc) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	home := os.Getenv("HOME")
	if p.Spec.User != "" {
		if u, err := user.Lookup(p.Spec.User); err == nil {
			home = u.HomeDir
		}
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"IMP_PROC=" + p.Metadata.Name,
		"IMP_DAEMON=" + p.Metadata.Labels[v1alpha1.LabelDaemonName],
		"IMP_REPLICA_INDEX=" + p.Metadata.Labels[v1alpha1.LabelReplicaIndex],
	}
	for _, e := range p.Spec.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	return env
}
