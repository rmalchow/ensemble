package main

import "strings"

// stringSlice is a repeatable string flag (e.g. --join host:port --join
// host:port), copied from media/cmd/mpvsync/probe.go. Each --flag value appends
// to the slice.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }
