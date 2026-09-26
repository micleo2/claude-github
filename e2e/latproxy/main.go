// Command latproxy is a TCP proxy that adds a fixed one-way delay in each
// direction without limiting throughput, to simulate a high-latency link in
// the e2e tests (the test kernel has no netem).
//
//	latproxy -listen :22001 -target 127.0.0.1:22000 -delay 10ms
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"time"
)

func main() {
	listen := flag.String("listen", ":22001", "listen address")
	target := flag.String("target", "127.0.0.1:22000", "address to forward to")
	delay := flag.Duration("delay", 10*time.Millisecond, "one-way delay")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer c.Close()
			t, err := net.Dial("tcp", *target)
			if err != nil {
				log.Print(err)
				return
			}
			defer t.Close()
			done := make(chan struct{}, 2)
			go pipe(t, c, *delay, done)
			go pipe(c, t, *delay, done)
			<-done
		}()
	}
}

type chunk struct {
	at   time.Time
	data []byte
}

// pipe copies src to dst, releasing each chunk delay after it was read.
func pipe(dst io.Writer, src io.Reader, delay time.Duration, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	q := make(chan chunk, 4096)
	go func() {
		defer close(q)
		for {
			buf := make([]byte, 64<<10)
			n, err := src.Read(buf)
			if n > 0 {
				q <- chunk{time.Now().Add(delay), buf[:n]}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range q {
		time.Sleep(time.Until(c.at))
		if _, err := dst.Write(c.data); err != nil {
			return
		}
	}
}
