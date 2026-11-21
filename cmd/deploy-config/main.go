// Command deploy-config serves a local page for writing .env.deploy before a
// deployment: it explains each setting, lists what still needs a value, runs
// the API's own startup validation, generates local keys and links to where
// third-party keys are created.
//
//	deploy-config [-file .env.deploy] [-addr 127.0.0.1:8099]
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xnqOS/go-starter-kit/kit/deployconfig"

	"starter-kit/internal/deploycatalog"
)

func main() {
	file := flag.String("file", ".env.deploy", "env file to read and write")
	example := flag.String("example", ".env.deploy.example", "starting point when the file does not exist")
	addr := flag.String("addr", "127.0.0.1:8099", "listen address; keep it on loopback")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		fail(err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		fail(fmt.Errorf("-addr must be a loopback address: the page shows and writes secrets"))
	}
	path, err := filepath.Abs(*file)
	if err != nil {
		fail(err)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(err)
	}
	srv := &deployconfig.Server{Path: path, Example: *example, Token: rand.Text(), Catalog: deploycatalog.New()}
	url := fmt.Sprintf("http://%s/?t=%s", listener.Addr(), srv.Token)
	fmt.Printf("Editing %s\nOpen %s\nOn a server over SSH: ssh -L 8099:127.0.0.1:8099 <server>, then open the same address locally.\nCtrl-C to stop.\n", path, url)
	server := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	fail(server.Serve(listener))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "deploy-config:", err)
	os.Exit(1)
}
