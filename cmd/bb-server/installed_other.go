//go:build !windows

package main

type settings struct{ repo, port string }

func installed() settings { return settings{} }
