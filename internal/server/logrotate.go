package server

import (
	"os"
	"time"
)

// launchd appends everything tailreach prints to tailreach.log, for as long as it's
// installed. Past maxLog it's moved to tailreach.log.1 (replacing the one before) and
// tailreach carries on in a new file: at most about 2 × maxLog on disk.
const maxLog = 5 << 20

func keepLogSmall(path string) {
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
			rotateOutput(path)
		}
		time.Sleep(time.Hour)
	}
}

// rotateFile moves a log that's grown past max aside, for files tailreach opens itself.
func rotateFile(path string, max int64) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > max {
		os.Rename(path, path+".1")
	}
}
