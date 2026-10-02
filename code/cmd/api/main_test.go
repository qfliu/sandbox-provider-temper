package main

import (
	"bufio"
	"net"
	"net/http"

	"testing"
	"time"
)

// A client that never finishes its headers must be disconnected, not held
// open forever (review finding M3).
func TestSlowHeadersAreCutOff(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.ReadHeaderTimeout = 300 * time.Millisecond // shortened for the test
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n")) // never sends the final blank line

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(conn).ReadString('\n')
	if err == nil {
		// Some Go versions answer 408 before closing; either way it ends.
		return
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server held the slow connection open for %s", time.Since(start))
	}
}

func TestServerLimitsAreSet(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.IdleTimeout != idleTimeout || srv.MaxHeaderBytes != maxHeaderBytes {
		t.Fatalf("limits not applied: %+v", srv)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatal("no global read/write timeout: long execs and uploads must not be cut off")
	}
}
