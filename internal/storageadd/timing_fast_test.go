//go:build !garage_integration

package storageadd

// fastWaits says the tests run against fakes, whose answers do not change with
// time, so every wait can be a few polls with no sleep.
const fastWaits = true
