package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/timothywashburn/ansibled/internal/model"
)

// streamState is the run state sent in "run" events.
type streamState struct {
	State       model.State `json:"state"`
	Finished    bool        `json:"finished"`
	Description string      `json:"description"`
	Started     string      `json:"started,omitempty"` // RFC 3339
	FinishedAt  string      `json:"finished_at,omitempty"`
	Duration    string      `json:"duration,omitempty"`
}

// logChunk is the payload of "log" events.
type logChunk struct {
	Text string `json:"text"`
	// Skipped is set on the first chunk when the start of a large log was
	// left out: the number of bytes skipped.
	Skipped int64 `json:"skipped,omitempty"`
}

// runStream streams a run's log and state as Server-Sent Events:
//
//	event: run   data: streamState   (at the start and on every change)
//	event: log   data: logChunk      id: byte offset after the chunk
//	event: done                      (the run finished and the log is complete)
//
// A reconnecting browser sends Last-Event-ID and resumes from that offset.
// A first connection to a log larger than the inline limit starts at its
// last logInlineLimit bytes.
func (s *Server) runStream(w http.ResponseWriter, r *http.Request, sess *session) {
	run := s.lookupRun(w, r, sess)
	if run == nil {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event, id string, v any) bool {
		var b bytes.Buffer
		if event != "" {
			fmt.Fprintf(&b, "event: %s\n", event)
		}
		if id != "" {
			fmt.Fprintf(&b, "id: %s\n", id)
		}
		data := []byte("{}")
		if v != nil {
			data, _ = json.Marshal(v)
		}
		fmt.Fprintf(&b, "data: %s\n\n", data)
		if _, err := w.Write(b.Bytes()); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	var offset int64 = -1
	if v, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && v >= 0 {
		offset = v
	}
	_, _ = fmt.Fprintf(w, "retry: 2000\n\n")

	last := s.streamState(run)
	if !send("run", "", last) {
		return
	}
	path := s.o.LogPath(run.ID)
	poll := time.NewTicker(s.streamPoll)
	defer poll.Stop()
	nextState := time.Now().Add(time.Second)
	nextPing := time.Now().Add(15 * time.Second)
	buf := make([]byte, streamChunk)

	for {
		finished := run.State.Finished()
		if !run.LogsExpired {
			var skipped int64
			if offset < 0 {
				offset, skipped = startOffset(path)
			}
			for {
				n, err := readFrom(path, offset, buf, finished)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					s.log.Warn("stream log", "run", run.ID, "err", err)
				}
				if n == 0 {
					break
				}
				offset += int64(n)
				text := string(bytes.ToValidUTF8(buf[:n], []byte("�")))
				if !send("log", strconv.FormatInt(offset, 10), logChunk{Text: text, Skipped: skipped}) {
					return
				}
				skipped = 0
				nextPing = time.Now().Add(15 * time.Second)
			}
		}
		if finished {
			send("done", "", nil)
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
		}
		if now := time.Now(); now.After(nextState) {
			nextState = now.Add(time.Second)
			fresh, err := s.o.Runs.GetRun(r.Context(), run.ID)
			if err == nil && fresh != nil {
				run = fresh
				if st := s.streamState(run); st != last {
					last = st
					if !send("run", "", st) {
						return
					}
				}
			}
		}
		if now := time.Now(); now.After(nextPing) {
			nextPing = now.Add(15 * time.Second)
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func (s *Server) streamState(r *model.Run) streamState {
	st := streamState{State: r.State, Finished: r.State.Finished(), Description: s.row(r).Description}
	if !r.StartedAt.IsZero() {
		st.Started = r.StartedAt.UTC().Format(time.RFC3339)
	}
	if !r.FinishedAt.IsZero() {
		st.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	if d := r.Duration(); d > 0 {
		st.Duration = s.row(r).Duration
	}
	return st
}

// startOffset is where a first connection starts: 0, or the first line
// boundary in the last logInlineLimit bytes of a large log (and how many
// bytes are skipped).
func startOffset(path string) (int64, int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= logInlineLimit {
		return 0, 0
	}
	off := st.Size() - logInlineLimit
	b := make([]byte, 64<<10)
	n, _ := f.ReadAt(b, off)
	if i := bytes.IndexByte(b[:n], '\n'); i >= 0 {
		off += int64(i + 1)
	}
	return off, off
}

// readFrom reads up to len(buf) bytes of path at offset into buf and returns
// how many to send: only complete lines unless all is set (the run has
// finished, so a last line without a newline is complete too). A single line
// longer than buf is cut at a UTF-8 boundary.
func readFrom(path string, offset int64, buf []byte, all bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if n == 0 || all {
		return n, nil
	}
	if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
		return i + 1, nil
	}
	if n < len(buf) {
		return 0, nil // a partial line: wait for the rest
	}
	i := n - 1
	for i > 0 && !utf8.RuneStart(buf[i]) {
		i--
	}
	if !utf8.FullRune(buf[i:n]) {
		n = i
	}
	return n, nil
}
