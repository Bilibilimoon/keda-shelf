package main

import (
	"bufio"
	"crypto/tls"
	"net"
	"time"
)

// loadTLSConfig returns nil when no certificate is configured, so the server
// keeps serving plain HTTP on the same port.
func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// sniffListener accepts both HTTPS and plain HTTP on one port so existing LAN
// clients (http://ip:port) keep working while the public address can use TLS.
type sniffListener struct {
	net.Listener
	cfg *tls.Config
}

func (l *sniffListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		conn, err := l.classify(c)
		if err != nil {
			c.Close()
			continue
		}
		return conn, nil
	}
}

func (l *sniffListener) classify(c net.Conn) (net.Conn, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, err
	}
	bc := &bufferedConn{Conn: c, r: br}
	if first[0] == 0x16 {
		return tls.Server(bc, l.cfg), nil
	}
	return bc, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
