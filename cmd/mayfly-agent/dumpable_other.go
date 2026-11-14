//go:build !linux

package main

// disableDumpable does nothing: only Linux exposes a process environment
// through /proc to a same-UID process. Sessions run in a Linux container.
func disableDumpable() error { return nil }
