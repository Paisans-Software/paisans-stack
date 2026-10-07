//go:build garage_integration

package storageadd

// fastWaits is off against real Garage containers: a node rejoining after a
// restart, or data moving after a layout change, takes real time, and a gate
// that polled without sleeping would fail a healthy cluster.
const fastWaits = false
