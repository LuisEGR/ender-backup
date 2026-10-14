package main

import (
	"io"
	"os"
	"strconv"
	"time"
)

// For testing slow networks (e.g. background uploads in the Simulator, where everything
// is local and instant): PHOTO_BACKUP_THROTTLE_KBPS limits how fast uploads are read.
var throttleKBps, _ = strconv.Atoi(os.Getenv("PHOTO_BACKUP_THROTTLE_KBPS"))

func throttled(r io.Reader) io.Reader {
	if throttleKBps <= 0 {
		return r
	}
	return &throttleReader{r: r, bytesPerSec: throttleKBps * 1000, start: time.Now()}
}

type throttleReader struct {
	r           io.Reader
	bytesPerSec int
	start       time.Time
	read        int
}

func (t *throttleReader) Read(p []byte) (int, error) {
	if len(p) > t.bytesPerSec/10 {
		p = p[:max(1, t.bytesPerSec/10)]
	}
	n, err := t.r.Read(p)
	t.read += n
	// Sleep until the average rate is back under the limit.
	due := time.Duration(float64(t.read) / float64(t.bytesPerSec) * float64(time.Second))
	if wait := due - time.Since(t.start); wait > 0 {
		time.Sleep(wait)
	}
	return n, err
}
