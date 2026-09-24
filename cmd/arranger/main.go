// Command arranger serves the agent orchestrator on a local web page.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"arranger/internal/orch"
	"arranger/internal/server"
	"arranger/internal/store"
	"arranger/internal/version"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7777", "listen address")
	data := flag.String("data", filepath.Join(home, ".arranger"), "data directory")
	parallel := flag.Int("parallel", 4, "max agent processes running at once")
	level := flag.String("log", "info", "log level: debug, info, warn or error")
	jsonLogs := flag.Bool("log-json", false, "log JSON lines instead of readable text")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Get())
		return
	}

	lg, err := newLogger(*level, *jsonLogs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer lg.Sync()
	zap.ReplaceGlobals(lg) // packages log through zap.L(); it's a no-op in tests
	defer zap.RedirectStdLog(lg)()

	st, err := store.Open(filepath.Join(*data, "arranger.db"))
	if err != nil {
		lg.Fatal("open store", zap.Error(err))
	}
	defer st.Close()
	o := orch.New(st, filepath.Join(*data, "worktrees"), *parallel)

	host, _, _ := net.SplitHostPort(*addr)
	ip := net.ParseIP(host)
	loopback := host == "localhost" || ip != nil && ip.IsLoopback()
	srv := &http.Server{Addr: *addr, Handler: server.New(st, o, loopback)}
	go func() {
		v := version.Get()
		lg.Info("arranger listening", zap.String("url", "http://"+*addr), zap.String("version", v.Version), zap.String("commit", v.Short()),
			zap.String("data", *data), zap.Int("parallel", *parallel))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			lg.Fatal("serve", zap.Error(err))
		}
	}()

	// on Ctrl-C, stop the agents instead of leaving their processes running without us
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	lg.Info("shutting down; stopping running agents")
	srv.Close() // live-update streams never end on their own, so no graceful Shutdown
	if !o.StopAll(10 * time.Second) {
		lg.Warn("some agents did not stop in time")
	}
}

// newLogger returns a zap logger writing to stderr: readable console lines by default, JSON on request.
func newLogger(level string, asJSON bool) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("-log: %w", err)
	}
	enc := zap.NewProductionEncoderConfig()
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	encoder := zapcore.NewJSONEncoder(enc)
	if !asJSON {
		enc.EncodeLevel = zapcore.CapitalLevelEncoder
		enc.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
		enc.EncodeDuration = zapcore.StringDurationEncoder
		encoder = zapcore.NewConsoleEncoder(enc)
	}
	return zap.New(zapcore.NewCore(encoder, zapcore.Lock(os.Stderr), lvl), zap.AddCaller()), nil
}
