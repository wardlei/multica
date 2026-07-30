//go:build fdlcrashprobe

package daemon

import (
	"os"
)

const fdlCrashProbeExitCode = 86

// newFDLDispatchCheckpoint exists only in an explicitly tagged acceptance
// binary. It terminates the process at one durable dispatch boundary.
func newFDLDispatchCheckpoint() func(point string) error {
	crashAfter := os.Getenv("MULTICA_FDL_CRASH_AFTER")
	if crashAfter != "after_acknowledgement" && crashAfter != "after_activation" {
		return nil
	}
	return func(point string) error {
		if point == crashAfter {
			os.Exit(fdlCrashProbeExitCode)
		}
		return nil
	}
}
