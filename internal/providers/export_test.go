package providers

// UnregisterDriver removes a driver a test registered. RegisterDriver panics on
// a duplicate name, so a test registering under a fixed name could run only
// once per process (go test -count=N re-runs it in the same process).
func UnregisterDriver(name string) {
	driverRegistryMu.Lock()
	defer driverRegistryMu.Unlock()
	delete(driverRegistry, name)
}
