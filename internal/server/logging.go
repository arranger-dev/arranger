package server

import (
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// statusWriter records the status code a handler wrote. It keeps Flush working for the SSE stream.
type statusWriter struct {
	http.ResponseWriter
	code int
	msg  []byte // start of an error response's body, for the log line
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if w.code >= 400 && len(w.msg) < 300 {
		w.msg = append(w.msg, b[:min(len(b), 300-len(w.msg))]...)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs each request once it's done. Changes and failures log at info/warn; the
// reads the page polls constantly (summary, events, static files) only at debug.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lg := zap.L()
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		lvl := zap.DebugLevel
		switch {
		case sw.code >= 500:
			lvl = zap.ErrorLevel
		case sw.code >= 400:
			lvl = zap.WarnLevel
		case r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/"):
			lvl = zap.InfoLevel
		}
		if ce := lg.Check(lvl, "http"); ce != nil {
			fs := []zap.Field{zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Int("status", sw.code),
				zap.Duration("took", time.Since(start))}
			if len(sw.msg) > 0 {
				fs = append(fs, zap.String("error", strings.TrimSpace(string(sw.msg))))
			}
			ce.Write(fs...)
		}
	})
}
