package config

// SetSyncFile replaces how WriteSecrets syncs its temporary file, and returns
// a function that restores it.
func SetSyncFile(f func(interface{ Sync() error }) error) (restore func()) {
	saved := syncFile
	syncFile = f
	return func() { syncFile = saved }
}
