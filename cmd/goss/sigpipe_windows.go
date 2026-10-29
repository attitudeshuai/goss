//go:build windows

package main

// Windows has no SIGPIPE: writing to a closed pipe or socket already fails
// with an ordinary error, so no runtime signal handling is needed there.
