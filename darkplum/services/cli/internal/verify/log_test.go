package verify

import "log"

func nopLogger() *log.Logger { return log.New(discard{}, "", 0) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
