// sp1b is a stub RTSP server for spike SP1b. It answers OPTIONS, challenges DESCRIBE with
// HTTP-style Basic auth, and logs whether the retried DESCRIBE carried the expected credential
// (never the password itself). After a correct credential it holds the connection for -hold,
// so the caller can inspect the client's command line, then replies 404.
package main

import (
	"bufio"
	"encoding/base64"
	"flag"
	"fmt"
	"net"
	"net/textproto"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18555", "listen address")
	user := flag.String("user", "fakeuser", "expected user")
	pass := flag.String("pass", "fakepass", "expected password")
	hold := flag.Duration("hold", 4*time.Second, "hold after a good credential")
	life := flag.Duration("life", 60*time.Second, "exit after this long")
	flag.Parse()
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	go func() { time.Sleep(*life); os.Exit(0) }()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go serve(c, *user, *pass, *hold)
	}
}

func serve(c net.Conn, user, pass string, hold time.Duration) {
	defer c.Close()
	r := textproto.NewReader(bufio.NewReader(c))
	for {
		reqLine, err := r.ReadLine()
		if err != nil {
			return
		}
		hdr, err := r.ReadMIMEHeader()
		if err != nil {
			return
		}
		method := strings.Fields(reqLine)[0]
		cseq := hdr.Get("Cseq")
		switch method {
		case "OPTIONS":
			fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nPublic: OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN\r\n\r\n", cseq)
		case "DESCRIBE":
			a := hdr.Get("Authorization")
			if a == "" {
				fmt.Println("DESCRIBE without Authorization, sending 401")
				fmt.Fprintf(c, "RTSP/1.0 401 Unauthorized\r\nCSeq: %s\r\nWWW-Authenticate: Basic realm=\"sp1b\"\r\n\r\n", cseq)
				continue
			}
			scheme, tok, _ := strings.Cut(a, " ")
			raw, _ := base64.StdEncoding.DecodeString(tok)
			u, p, _ := strings.Cut(string(raw), ":")
			fmt.Printf("DESCRIBE with Authorization scheme=%s user=%q user_ok=%v pass_ok=%v\n", scheme, u, u == user, p == pass)
			if u == user && p == pass {
				time.Sleep(hold)
			}
			fmt.Fprintf(c, "RTSP/1.0 404 Not Found\r\nCSeq: %s\r\n\r\n", cseq)
			return
		default:
			fmt.Fprintf(c, "RTSP/1.0 405 Method Not Allowed\r\nCSeq: %s\r\n\r\n", cseq)
		}
	}
}
