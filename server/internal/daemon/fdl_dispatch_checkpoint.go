//go:build !fdlcrashprobe

package daemon

// newFDLDispatchCheckpoint keeps production daemons free of fault injection.
func newFDLDispatchCheckpoint() func(point string) error {
	return nil
}
