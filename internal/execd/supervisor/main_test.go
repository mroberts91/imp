// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package supervisor

import (
	"os"
	"testing"

	"github.com/mroberts91/imp/internal/execd/childsetup"
)

// TestMain doubles this test binary as the childsetup shim: spawns go
// through /proc/self/exe (M6-a always-shim), which in tests is the test
// binary itself. Without this hook a spawned Proc would run the test suite
// instead of its command. Mirrors childsetup.MaybeRun's placement at the
// top of impd's main.
func TestMain(m *testing.M) {
	childsetup.MaybeRun()
	os.Exit(m.Run())
}
