package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// Photos are converted in a helper process (this same program, started with
// PHOTO_BACKUP_CONVERT_WORKER=1), not in the server: the encoder runs as WebAssembly code
// that Go can't interrupt, so while it ran in the server, garbage collection had to wait
// for it and every request stalled for seconds. The helper runs at low priority and
// converts one photo per goroutine; the server stays at normal priority and responsive.

const convertWorkerEnv = "PHOTO_BACKUP_CONVERT_WORKER"

type convertJob struct {
	ID      int64  `json:"id"`
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Quality int    `json:"quality"`
}

type convertResult struct {
	ID    int64  `json:"id"`
	Size  int64  `json:"size"`
	Error string `json:"error,omitempty"`
}

// runConvertWorker is the helper process: jobs come in on stdin, results go out on
// stdout, one JSON object per line. It exits when stdin closes (the server is gone).
func runConvertWorker() {
	setLowPriority()
	out := json.NewEncoder(os.Stdout)
	var mu sync.Mutex
	var wg sync.WaitGroup
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	for in.Scan() {
		var job convertJob
		if err := json.Unmarshal(in.Bytes(), &job); err != nil {
			continue
		}
		wg.Add(1)
		go func() { // the server decides how many run at once
			defer wg.Done()
			size, err := convertToAVIF(job.Src, job.Dst, job.Quality)
			res := convertResult{ID: job.ID, Size: size}
			if err != nil {
				res.Error = err.Error()
			}
			mu.Lock()
			out.Encode(res)
			mu.Unlock()
		}()
	}
	wg.Wait()
}

// convertHelper is the server's handle on the helper process, started on first use and
// restarted if it exits.
type convertHelper struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	nextID  int64
	pending map[int64]chan convertResult
}

var helper = &convertHelper{}

func (h *convertHelper) start() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), convertWorkerEnv+"=1")
	cmd.Stderr = os.Stderr
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	h.cmd, h.stdin, h.pending = cmd, stdin, map[int64]chan convertResult{}
	go h.read(cmd, stdout)
	return nil
}

// read delivers results; when the helper exits, conversions still waiting fail.
func (h *convertHelper) read(cmd *exec.Cmd, stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var res convertResult
		if json.Unmarshal(sc.Bytes(), &res) != nil {
			continue
		}
		h.mu.Lock()
		ch := h.pending[res.ID]
		delete(h.pending, res.ID)
		h.mu.Unlock()
		if ch != nil {
			ch <- res
		}
	}
	err := cmd.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == cmd {
		for id, ch := range h.pending {
			ch <- convertResult{ID: id, Error: fmt.Sprintf("converter stopped (%v)", err)}
		}
		h.cmd, h.stdin, h.pending = nil, nil, nil
	}
}

// convert converts src to an AVIF at dst in the helper process and returns its size.
func (h *convertHelper) convert(src, dst string, quality int) (int64, error) {
	h.mu.Lock()
	if h.cmd == nil {
		if err := h.start(); err != nil {
			h.mu.Unlock()
			return 0, fmt.Errorf("start converter: %w", err)
		}
	}
	h.nextID++
	job := convertJob{ID: h.nextID, Src: src, Dst: dst, Quality: quality}
	ch := make(chan convertResult, 1)
	h.pending[job.ID] = ch
	line, _ := json.Marshal(job)
	_, err := h.stdin.Write(append(line, '\n'))
	h.mu.Unlock()
	if err != nil {
		h.mu.Lock()
		if h.pending != nil {
			delete(h.pending, job.ID)
		}
		h.mu.Unlock()
		return 0, fmt.Errorf("send to converter: %w", err)
	}
	res := <-ch
	if res.Error != "" {
		return 0, errors.New(res.Error)
	}
	return res.Size, nil
}

// stop ends the helper process (it exits when its input closes).
func (h *convertHelper) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stdin != nil {
		h.stdin.Close()
	}
}
