// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package childsetup

import "errors"

// apply is linux-only; impd releases target linux and this stub only keeps
// cross-platform builds compiling. A non-linux shim invocation exits 126.
func apply(*Payload) error {
	return errors.New("child setup is only supported on linux")
}
