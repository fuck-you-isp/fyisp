//go:build !linux && !windows && !darwin

package main

func whoami() string    { return "?" }
func methods() []method { return nil }
