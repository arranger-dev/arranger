// Command arranger serves the agent orchestrator on a local web page.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"arranger/internal/orch"
	"arranger/internal/server"
	"arranger/internal/store"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7777", "listen address")
	data := flag.String("data", filepath.Join(home, ".arranger"), "data directory")
	parallel := flag.Int("parallel", 4, "max agent processes running at once")
	flag.Parse()

	st, err := store.Open(filepath.Join(*data, "arranger.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	o := orch.New(st, filepath.Join(*data, "worktrees"), *parallel)

	host, _, _ := net.SplitHostPort(*addr)
	ip := net.ParseIP(host)
	loopback := host == "localhost" || ip != nil && ip.IsLoopback()
	log.Printf("arranger on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, server.New(st, o, loopback)))
}
