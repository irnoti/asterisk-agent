package main

import (
	"os"
	"runtime"
	"time"
)

func timeAfterSecond() <-chan time.Time { return time.After(time.Second) }

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func goarch() string { return runtime.GOARCH }
