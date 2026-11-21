//go:build windows

package main

func dirOwner(string) (int, int, bool) { return 0, 0, false }

func adoptOwner(string, int, int) {}
