package latency

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Attempt captures httptrace callbacks without replacing the HTTP transport.
type Attempt struct {
	mu      sync.Mutex
	span    *Span
	ctx     context.Context
	dns     time.Time
	tls     time.Time
	write   time.Time
	connect map[string]time.Time
	reused  *bool
}

func StartAttempt(ctx context.Context, number int) (context.Context, *Attempt) {
	if FromContext(ctx) == nil {
		return ctx, nil
	}
	ctx = context.WithValue(ctx, attemptKey{}, number)
	a := &Attempt{span: Begin(ctx, HTTP, "attempt"), ctx: ctx}
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			a.mu.Lock()
			a.dns = time.Now()
			a.mu.Unlock()
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.add(DNS, a.dns, info.Err)
		},
		ConnectStart: func(network, address string) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.connect == nil {
				a.connect = make(map[string]time.Time)
			}
			a.connect[network+"\x00"+address] = time.Now()
		},
		ConnectDone: func(network, address string, err error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			key := network + "\x00" + address
			a.add(Connect, a.connect[key], err)
			delete(a.connect, key)
		},
		TLSHandshakeStart: func() {
			a.mu.Lock()
			a.tls = time.Now()
			a.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.add(TLS, a.tls, err)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			a.mu.Lock()
			defer a.mu.Unlock()
			value := info.Reused
			a.reused = &value
			a.write = time.Now()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.add(RequestWrite, a.write, info.Err)
		},
		GotFirstResponseByte: func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.add(FirstByte, a.span.started, nil)
		},
	}
	return httptrace.WithClientTrace(ctx, trace), a
}

func (a *Attempt) add(stage Stage, started time.Time, err error) {
	if started.IsZero() {
		return
	}
	e := base(a.ctx, stage, "network")
	result(&e, time.Since(started), err)
	a.span.recorder.add(e)
}

func (a *Attempt) End(response *http.Response, err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.span.entry.Reused = a.reused
	if response != nil {
		switch response.Proto {
		case "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/3.0":
			a.span.entry.Protocol = response.Proto
		}
		a.span.entry.Status = response.StatusCode
		if err == nil && response.StatusCode != http.StatusOK {
			err = errors.New("HTTP status")
		}
	}
	a.span.End(err)
}
