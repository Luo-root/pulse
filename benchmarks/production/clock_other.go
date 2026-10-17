//go:build !windows

package main

import "time"

func benchNow() time.Time                      { return time.Now() }
func benchSince(start time.Time) time.Duration { return time.Since(start) }
