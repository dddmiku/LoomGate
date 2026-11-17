package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNetworkCheckReportsDNSAndTLSWithoutCredentials(t *testing.T) {
	notAfter := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	var out bytes.Buffer
	code := runNetworkCheck(context.Background(), "example.test",
		func(context.Context, string) ([]string, error) { return []string{"192.0.2.1", "192.0.2.2"}, nil },
		func(_ context.Context, host string) (tls.ConnectionState, error) {
			if host != "example.test" {
				t.Fatalf("dial host = %q", host)
			}
			return tls.ConnectionState{
				Version:          tls.VersionTLS13,
				PeerCertificates: []*x509.Certificate{{NotAfter: notAfter}},
			}, nil
		}, &out)
	if code != 0 {
		t.Fatalf("code = %d out=%s", code, out.String())
	}
	var got networkCheckResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.DNSAddresses != 2 || got.TLSVersion != "TLS 1.3" ||
		got.TLSNotAfter != notAfter.Format(time.RFC3339) || got.Host != "example.test" {
		t.Fatalf("result = %+v", got)
	}
	if strings.Contains(out.String(), "192.0.2.1") {
		t.Fatal("addresses must not leak into the report")
	}
}

func TestNetworkCheckFailsClosedOnDNSOrTLS(t *testing.T) {
	for name, fixture := range map[string]struct {
		lookup func(context.Context, string) ([]string, error)
		dial   func(context.Context, string) (tls.ConnectionState, error)
	}{
		"dns": {lookup: func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver") }},
		"tls": {
			lookup: func(context.Context, string) ([]string, error) { return []string{"192.0.2.1"}, nil },
			dial: func(context.Context, string) (tls.ConnectionState, error) {
				return tls.ConnectionState{}, errors.New("handshake failed")
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			code := runNetworkCheck(context.Background(), "example.test", fixture.lookup, fixture.dial, &out)
			if code == 0 {
				t.Fatalf("expected failure, out=%s", out.String())
			}
			var got networkCheckResult
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.OK {
				t.Fatalf("ok must stay false: %+v", got)
			}
			if name == "dns" && got.DNSError == "" || name == "tls" && got.TLSError == "" {
				t.Fatalf("missing failure detail: %+v", got)
			}
		})
	}
}

func TestNetworkCheckFlagIsExact(t *testing.T) {
	if !isNetworkCheckArg([]string{"--loomgate-network-check"}) {
		t.Fatal("exact flag not recognized")
	}
	for _, args := range [][]string{nil, {}, {"--loomgate-build-info"}, {"--loomgate-network-check", "x"}, {"x", "--loomgate-network-check"}} {
		if isNetworkCheckArg(args) {
			t.Fatalf("args %v must not trigger the network check", args)
		}
	}
}
