package outbound

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"testing"
)

func TestNoProxy(t *testing.T) {
	c := Config{HTTP: "http://proxy.example:3128", HTTPS: "https://proxy.example:443", NoProxy: ".internal.example,example.com:8443,10.0.0.0/8,[2001:db8::1]:443"}
	for _, tt := range []struct {
		host   string
		bypass bool
	}{{"app.internal.example:443", true}, {"internal.example:443", false}, {"example.com:8443", true}, {"example.com:443", false}, {"badexample.com:8443", false}, {"10.2.3.4:443", true}, {"[2001:db8::1]:443", true}, {"[2001:db8::2]:443", false}, {"localhost:443", true}, {"127.0.0.1:443", true}} {
		proxy, err := c.For("https", tt.host)
		if err != nil || (proxy == nil) != tt.bypass {
			t.Fatalf("host=%s proxy=%v err=%v", tt.host, proxy, err)
		}
	}
	c.NoProxy = "*"
	proxy, err := c.For("http", "public.example:80")
	if err != nil || proxy != nil {
		t.Fatal(proxy, err)
	}
}

func TestTunnelUsesValidatedIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan *http.Request, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		r, e := http.ReadRequest(bufio.NewReader(conn))
		if e != nil {
			return
		}
		requests <- r
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	}()
	proxy, _ := url.Parse("http://user:secret@" + listener.Addr().String())
	conn, err := Tunnel(context.Background(), proxy, "tcp", "93.184.216.34:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	r := <-requests
	if r.Method != "CONNECT" || r.Host != "93.184.216.34:443" || r.Header.Get("Proxy-Authorization") == "" {
		t.Fatal("proxy did not receive validated destination and authentication")
	}
}
