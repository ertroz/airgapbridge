package proxy

import (
	"bufio"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestProxyInterceptsSitHTTPAndWebSocketAndPassesThroughOthers(t *testing.T) {
	// 1. Mock local dev server (HTTP + WebSocket upgrade echo)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()

			sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			accept := base64.StdEncoding.EncodeToString(sum[:])
			_, _ = fmt.Fprintf(conn,
				"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\nHMR_OK",
				accept,
			)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"url":    r.URL.RequestURI(),
			"host":   r.Host,
			"origin": r.Header.Get("Origin"),
		})
	}))
	defer localSrv.Close()

	localURL, _ := url.Parse(localSrv.URL)
	var localPort int
	_, _ = fmt.Sscanf(localURL.Port(), "%d", &localPort)

	// 2. Mock external TCP server for passthrough test
	extLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer extLn.Close()
	go func() {
		for {
			c, err := extLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 64)
				n, _ := conn.Read(buf)
				_, _ = conn.Write([]byte("PASSTHROUGH:" + string(buf[:n])))
			}(c)
		}
	}()

	// 3. Start proxy on ephemeral port
	srv, err := Start(Options{
		SitURL:      "https://sit-app.company.com",
		LocalPort:   localPort,
		ProxyPort:   0,
		BypassPaths: []string{"^/api/auth/.*"},
	})
	if err != nil {
		t.Fatalf("proxy.Start failed: %v", err)
	}
	defer srv.Close()

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", srv.ProxyPort)

	// Test A: CONNECT sit-app.company.com:443 -> TLS -> HTTP request
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(conn, "CONNECT sit-app.company.com:443 HTTP/1.1\r\nHost: sit-app.company.com:443\r\n\r\n")
	br := bufio.NewReader(conn)
	statusLine, _ := br.ReadString('\n')
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200 Connection Established, got %q", statusLine)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         "sit-app.company.com",
		InsecureSkipVerify: true,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake failed: %v", err)
	}
	_, _ = fmt.Fprintf(tlsConn, "GET /hello?x=1 HTTP/1.1\r\nHost: sit-app.company.com\r\nOrigin: https://sit-app.company.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	var payload map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	_ = resp.Body.Close()
	_ = tlsConn.Close()

	if payload["url"] != "/hello?x=1" || payload["host"] != "sit-app.company.com" || payload["origin"] != "https://sit-app.company.com" {
		t.Fatalf("unexpected proxied payload: %+v", payload)
	}

	// Test B: CONNECT sit-app.company.com:443 -> TLS -> WebSocket Upgrade (HMR)
	wsRawConn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(wsRawConn, "CONNECT sit-app.company.com:443 HTTP/1.1\r\nHost: sit-app.company.com:443\r\n\r\n")
	wsBr := bufio.NewReader(wsRawConn)
	_, _ = wsBr.ReadString('\n')
	for {
		line, _ := wsBr.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}
	wsTLS := tls.Client(wsRawConn, &tls.Config{
		ServerName:         "sit-app.company.com",
		InsecureSkipVerify: true,
	})
	_, _ = fmt.Fprintf(wsTLS,
		"GET /_next/webpack-hmr HTTP/1.1\r\nHost: sit-app.company.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n",
	)
	wsRespBytes, _ := io.ReadAll(wsTLS)
	_ = wsTLS.Close()
	wsRespStr := string(wsRespBytes)
	if !strings.Contains(wsRespStr, "101 Switching Protocols") || !strings.Contains(wsRespStr, "HMR_OK") {
		t.Fatalf("expected 101 Switching Protocols + HMR_OK, got:\n%s", wsRespStr)
	}

	// Test C: CONNECT external host -> pure TCP passthrough
	ptConn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	extAddr := extLn.Addr().String()
	_, _ = fmt.Fprintf(ptConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", extAddr, extAddr)
	ptBr := bufio.NewReader(ptConn)
	_, _ = ptBr.ReadString('\n')
	for {
		line, _ := ptBr.ReadString('\n')
		if line == "\r\n" || line == "" {
			break
		}
	}
	_, _ = ptConn.Write([]byte("PING"))
	ptOut, _ := io.ReadAll(ptBr)
	_ = ptConn.Close()
	if string(ptOut) != "PASSTHROUGH:PING" {
		t.Fatalf("expected PASSTHROUGH:PING, got %q", string(ptOut))
	}
}
