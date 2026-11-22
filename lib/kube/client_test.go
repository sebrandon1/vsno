package kube

import (
	"net/http"
	"testing"

	"k8s.io/client-go/rest"
)

// A config that already wraps its transport keeps that wrapper, and the trace
// goes outermost.
func TestWrapTransport(t *testing.T) {
	wrapped := false
	cfg := &rest.Config{Host: "https://example.test"}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		wrapped = true
		return rt
	}
	wrapTransport(cfg)
	if rt := cfg.WrapTransport(http.DefaultTransport); rt == nil {
		t.Fatal("WrapTransport returned nil")
	}
	if !wrapped {
		t.Error("the existing wrapper was dropped")
	}
}

func TestWrapTransportWithoutOne(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.test"}
	wrapTransport(cfg)
	if cfg.WrapTransport == nil {
		t.Fatal("WrapTransport was not installed")
	}
	if rt := cfg.WrapTransport(http.DefaultTransport); rt == nil {
		t.Fatal("WrapTransport returned nil")
	}
}
