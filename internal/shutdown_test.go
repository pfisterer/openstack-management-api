package app

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// A request in flight when the stop signal comes is still answered; the server then ends without error and takes no new connections.
func TestServeUntilDrains(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("done"))
	})
	server := newHTTPServer(addr, handler)
	stop, signal := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveUntil(stop, server, 5*time.Second) }()

	var resp *http.Response
	got := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			if resp, err = http.Get("http://" + addr + "/"); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		got <- err
	}()
	<-started
	signal()
	if err := <-got; err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request in flight was not answered: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("serveUntil: %v", err)
	}
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Fatal("the server still takes connections after the shutdown")
	}
}
