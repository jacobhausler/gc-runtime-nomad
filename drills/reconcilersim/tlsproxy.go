package reconcilersim

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// TLSProxy fronts a plain-HTTP local listener for a real mTLS Nomad API, so
// the pack CLI subprocess — which has zero TLS/client-cert configuration by
// design (NRT-P1-04, reaffirmed at NRT-P2-05) — can still reach the T1 lab
// cluster's NRT-P2-02 mTLS baseline. It presents the client cert/key on the
// upstream leg and trusts only the given CA, the same way this package's
// own Client does for its direct reads; unlike the throwaway, gitignored
// proxy NRT-P2-05's reconciliation pass improvised for one manual run, this
// is the harness's own reusable, committed version of the same technique.
type TLSProxy struct {
	listener net.Listener
	srv      *http.Server
}

// StartTLSProxy starts listening on 127.0.0.1:0 and proxies every request
// to targetAddr (an https:// Nomad API base URL) over mTLS using cfg. The
// caller must Close it when the drill finishes.
func StartTLSProxy(targetAddr string, cfg TLSConfig) (*TLSProxy, error) {
	target, err := url.Parse(targetAddr)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy target %q: %w", targetAddr, err)
	}
	transport, err := tlsTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("building proxy mTLS transport: %w", err)
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = transport

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for mTLS proxy: %w", err)
	}
	srv := &http.Server{Handler: rp}
	go func() { _ = srv.Serve(ln) }()

	return &TLSProxy{listener: ln, srv: srv}, nil
}

// URL is the local, plain-HTTP address the pack CLI subprocess should be
// pointed at (GC_NOMAD_ADDR) instead of the real mTLS endpoint.
func (p *TLSProxy) URL() string {
	return fmt.Sprintf("http://%s", p.listener.Addr().String())
}

// Close shuts the proxy down. Safe to call once; a drill defers it right
// after StartTLSProxy succeeds.
func (p *TLSProxy) Close() error {
	return p.srv.Close()
}
