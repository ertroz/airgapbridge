package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Options struct {
	SitURL      string
	LocalPort   int
	ProxyPort   int
	BypassPaths []string
}

type Server struct {
	ProxyPort      int
	forwardServer  *http.Server
	internalServer *http.Server
}

func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err1, err2 error
	if s.forwardServer != nil {
		err1 = s.forwardServer.Shutdown(ctx)
	}
	if s.internalServer != nil {
		err2 = s.internalServer.Shutdown(ctx)
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func Start(opts Options) (*Server, error) {
	sitParsed, err := url.Parse(opts.SitURL)
	if err != nil {
		return nil, fmt.Errorf("invalid sitUrl: %w", err)
	}
	sitHostname := strings.ToLower(sitParsed.Hostname())
	sitOrigin := fmt.Sprintf("%s://%s", sitParsed.Scheme, sitParsed.Host)

	localTarget, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", opts.LocalPort))
	if err != nil {
		return nil, err
	}

	bypassRegexes := make([]*regexp.Regexp, 0, len(opts.BypassPaths))
	for _, p := range opts.BypassPaths {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid bypassPath regex %q: %w", p, err)
		}
		bypassRegexes = append(bypassRegexes, re)
	}

	shouldBypass := func(reqURL string) bool {
		pathOnly := reqURL
		if idx := strings.IndexByte(reqURL, '?'); idx != -1 {
			pathOnly = reqURL[:idx]
		}
		for _, re := range bypassRegexes {
			if re.MatchString(pathOnly) || re.MatchString(reqURL) {
				return true
			}
		}
		return false
	}

	localProxy := httputil.NewSingleHostReverseProxy(localTarget)
	origLocalDirector := localProxy.Director
	localProxy.Director = func(req *http.Request) {
		origHost := req.Host
		origOrigin := req.Header.Get("Origin")
		origLocalDirector(req)
		if origHost != "" {
			req.Host = origHost
		}
		if origOrigin != "" {
			req.Header.Set("Origin", origOrigin)
		}
		req.Header.Set("X-Forwarded-Host", sitParsed.Host)
		req.Header.Set("X-Forwarded-Proto", sitParsed.Scheme)
	}
	localProxy.ModifyResponse = func(resp *http.Response) error {
		loc := resp.Header.Get("Location")
		if loc != "" {
			localHTTP := fmt.Sprintf("http://localhost:%d", opts.LocalPort)
			localIP := fmt.Sprintf("http://127.0.0.1:%d", opts.LocalPort)
			if strings.HasPrefix(loc, localHTTP) {
				resp.Header.Set("Location", sitOrigin+strings.TrimPrefix(loc, localHTTP))
			} else if strings.HasPrefix(loc, localIP) {
				resp.Header.Set("Location", sitOrigin+strings.TrimPrefix(loc, localIP))
			}
		}
		return nil
	}
	localProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "[airgapbridge] Local dev proxy error: %v", err)
	}

	sitProxy := httputil.NewSingleHostReverseProxy(sitParsed)
	origSitDirector := sitProxy.Director
	sitProxy.Director = func(req *http.Request) {
		origHost := req.Host
		origSitDirector(req)
		if origHost != "" {
			req.Host = origHost
		}
	}
	sitProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "[airgapbridge] SIT upstream proxy error: %v", err)
	}

	cert, err := generateSelfSignedCert(sitHostname)
	if err != nil {
		return nil, fmt.Errorf("failed to generate TLS cert: %w", err)
	}

	internalMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldBypass(r.URL.RequestURI()) {
			sitProxy.ServeHTTP(w, r)
			return
		}
		localProxy.ServeHTTP(w, r)
	})

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	internalLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to bind internal TLS server: %w", err)
	}
	internalAddr := internalLn.Addr().String()

	internalServer := &http.Server{
		Handler: internalMux,
	}
	go func() {
		_ = internalServer.Serve(internalLn)
	}()

	forwardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			handleConnect(w, r, sitHostname, internalAddr)
			return
		}
		handlePlainHTTP(w, r, sitHostname, shouldBypass, localProxy)
	})

	forwardAddr := fmt.Sprintf("127.0.0.1:%d", opts.ProxyPort)
	forwardLn, err := net.Listen("tcp", forwardAddr)
	if err != nil {
		_ = internalServer.Close()
		return nil, fmt.Errorf("failed to bind proxy on %s: %w", forwardAddr, err)
	}

	forwardServer := &http.Server{
		Handler: forwardHandler,
	}
	go func() {
		_ = forwardServer.Serve(forwardLn)
	}()

	actualPort := forwardLn.Addr().(*net.TCPAddr).Port
	return &Server{
		ProxyPort:      actualPort,
		forwardServer:  forwardServer,
		internalServer: internalServer,
	}, nil
}

func handleConnect(w http.ResponseWriter, r *http.Request, sitHostname, internalAddr string) {
	authority := r.Host
	if authority == "" {
		authority = r.RequestURI
	}
	targetHost := extractHost(authority)
	dialTarget := authority
	if strings.EqualFold(targetHost, sitHostname) {
		dialTarget = internalAddr
	}

	upstreamConn, err := net.DialTimeout("tcp", dialTarget, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream dial failed: %v", err), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstreamConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, buf, err := hijacker.Hijack()
	if err != nil {
		_ = upstreamConn.Close()
		return
	}

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\nProxy-agent: airgapbridge\r\n\r\n"))

	// Flush any buffered bytes from client reader into upstream
	if buf != nil && buf.Reader.Buffered() > 0 {
		buffered := make([]byte, buf.Reader.Buffered())
		n, _ := buf.Read(buffered)
		if n > 0 {
			_, _ = upstreamConn.Write(buffered[:n])
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstreamConn, clientConn)
		if tc, ok := upstreamConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, upstreamConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
	_ = upstreamConn.Close()
	_ = clientConn.Close()
}

func handlePlainHTTP(
	w http.ResponseWriter,
	r *http.Request,
	sitHostname string,
	shouldBypass func(string) bool,
	localProxy *httputil.ReverseProxy,
) {
	if strings.EqualFold(r.URL.Hostname(), sitHostname) && !shouldBypass(r.URL.RequestURI()) {
		r.URL.Scheme = ""
		r.URL.Host = ""
		localProxy.ServeHTTP(w, r)
		return
	}

	req := r.Clone(r.Context())
	req.RequestURI = ""
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func extractHost(authority string) string {
	host, _, err := net.SplitHostPort(authority)
	if err == nil {
		return host
	}
	return strings.Trim(authority, "[]")
}

func generateSelfSignedCert(hostname string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: hostname,
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}, nil
}
