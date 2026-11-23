package worker

import (
	"bufio"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func runTransportRouteProbe() error {
	if os.Args[2] != "tcp" && os.Args[2] != "udp" {
		return fmt.Errorf("invalid fixture transport")
	}
	conn, err := net.DialTimeout(os.Args[2], os.Args[3], 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("probe\n")); err != nil {
		return err
	}
	data := make([]byte, 128)
	n, err := conn.Read(data)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(string(data[:n]), ":probe\n") {
		return fmt.Errorf("unexpected transport response")
	}
	return nil
}

func runTransportRouteFixture() error {
	marker := os.Args[2]
	tcp, err := net.Listen("tcp", ":9000")
	if err != nil {
		return err
	}
	defer tcp.Close()
	certificate, err := tls.LoadX509KeyPair("/fixture.crt", "/fixture.key")
	if err != nil {
		return err
	}
	encrypted, err := tls.Listen("tcp", ":9002", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	defer encrypted.Close()
	udp, err := net.ListenPacket("udp", ":9001")
	if err != nil {
		return err
	}
	defer udp.Close()
	for _, listener := range []net.Listener{tcp, encrypted} {
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(10 * time.Second))
					data, err := bufio.NewReader(conn).ReadString('\n')
					if err == nil {
						fmt.Fprint(conn, marker+":"+data)
					}
				}()
			}
		}()
	}
	go func() {
		data := make([]byte, 1024)
		for {
			n, peer, err := udp.ReadFrom(data)
			if err != nil {
				return
			}
			udp.WriteTo(append([]byte(marker+":"), data[:n]...), peer)
		}
	}()
	return http.ListenAndServe(":8080", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(2 * time.Second)
		}
		if r.URL.Path != "/ws" {
			fmt.Fprint(w, marker)
			return
		}
		key := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(key[:]))
		if rw.Flush() != nil {
			return
		}
		for {
			var header [2]byte
			if _, err := io.ReadFull(rw, header[:]); err != nil {
				return
			}
			if header[0] != 0x81 || header[1]&0x80 == 0 || header[1]&0x7f > 125 {
				return
			}
			var mask [4]byte
			if _, err := io.ReadFull(rw, mask[:]); err != nil {
				return
			}
			data := make([]byte, int(header[1]&0x7f))
			if _, err := io.ReadFull(rw, data); err != nil {
				return
			}
			for i := range data {
				data[i] ^= mask[i%4]
			}
			if _, err := rw.Write(append([]byte{0x81, byte(len(data))}, data...)); err != nil {
				return
			}
			if rw.Flush() != nil {
				return
			}
		}
	}))
}
