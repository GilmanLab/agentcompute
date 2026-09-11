package compute

const execOutputLimit = 64 * 1024

// drainingWriter keeps at most limit bytes and discards the rest without
// blocking the producer, so the Incus exec websocket can finish.
type drainingWriter struct {
	limit int
	buf   []byte
	trunc bool
}

func newDrainingWriter(limit int) *drainingWriter {
	return &drainingWriter{limit: limit}
}

func (w *drainingWriter) Write(p []byte) (int, error) {
	if remain := w.limit - len(w.buf); remain > 0 {
		if len(p) <= remain {
			w.buf = append(w.buf, p...)
			return len(p), nil
		}
		w.buf = append(w.buf, p[:remain]...)
		w.trunc = true
		return len(p), nil
	}
	if len(p) > 0 {
		w.trunc = true
	}
	return len(p), nil
}

func (w *drainingWriter) String() string {
	return string(w.buf)
}

func (w *drainingWriter) Truncated() bool {
	return w.trunc
}
