// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/mroberts91/imp/api/v1alpha1"
)

// buildCmd constructs the Cmd for p without starting it. stdout/stderr must
// already be open (log writers). Environment is explicit — never inherits
// impd's env (supervisor information-leak discipline).
func buildCmd(p *v1alpha1.Proc, stdout, stderr io.Writer) (*exec.Cmd, error) {
	if len(p.Spec.Command) == 0 {
		return nil, fmt.Errorf("proc %s: empty command", p.Metadata.Name)
	}
	cmd := exec.Command(p.Spec.Command[0], p.Spec.Command[1:]...) //nolint:gosec // command comes from validated Proc spec
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Dir = p.Spec.WorkingDir
	cmd.Env = buildEnv(p)

	attr := &syscall.SysProcAttr{Setpgid: true}
	if p.Spec.User != "" || p.Spec.Group != "" {
		cred, err := resolveCredential(p.Spec.User, p.Spec.Group)
		if err != nil {
			return nil, err
		}
		attr.Credential = cred
	}
	cmd.SysProcAttr = attr
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

func resolveCredential(userName, groupName string) (*syscall.Credential, error) {
	cred := &syscall.Credential{}
	if userName != "" {
		u, err := user.Lookup(userName)
		if err != nil {
			return nil, fmt.Errorf("looking up user %q: %w", userName, err)
		}
		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing uid for %q: %w", userName, err)
		}
		cred.Uid = uint32(uid)
		if groupName == "" {
			gid, err := strconv.ParseUint(u.Gid, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("parsing gid for %q: %w", userName, err)
			}
			cred.Gid = uint32(gid)
		}
	}
	if groupName != "" {
		g, err := user.LookupGroup(groupName)
		if err != nil {
			return nil, fmt.Errorf("looking up group %q: %w", groupName, err)
		}
		gid, err := strconv.ParseUint(g.Gid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing gid for %q: %w", groupName, err)
		}
		cred.Gid = uint32(gid)
	}
	return cred, nil
}
