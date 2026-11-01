package export

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ContractorKeith/machine-monitor/internal/agent"
)

func TestExportDoesNotSendKeyBeforeProof(t *testing.T) {
	for _, operation := range []string{"devices", "history"} {
		t.Run(operation, func(t *testing.T) {
			var keyed atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					keyed.Add(1)
				}
				w.Write([]byte(`{"app":"device-monitor","proof":"wrong","points":[]}`))
			}))
			defer srv.Close()
			h := Hub{URL: srv.URL, Key: "review-only-key"}
			var err error
			if operation == "devices" {
				_, err = h.Devices(context.Background())
			} else {
				_, err = h.Points(context.Background(), "dev", time.Now().Add(-time.Hour), time.Now())
			}
			if keyed.Load() != 0 {
				t.Fatalf("key sent %d times to an unproven server", keyed.Load())
			}
			if err == nil {
				t.Fatal("unproven server accepted")
			}
		})
	}
}

func TestExportBypassesProxyAndRefusesRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.Write([]byte(`[]`))
	}))
	defer target.Close()
	u, _ := url.Parse(target.URL)
	for _, redirect := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/info" {
				fmt.Fprintf(w, `{"app":"device-monitor","proof":%q}`, agent.Proof("key", r.URL.Query().Get("nonce")))
				return
			}
			if redirect {
				http.Redirect(w, r, target.URL, http.StatusFound)
				return
			}
			w.Write([]byte(`[]`))
		}))
		h := Hub{URL: srv.URL, Key: "key", Client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}}
		_, err := h.Devices(context.Background())
		if (err != nil) != redirect {
			t.Fatalf("redirect=%v error=%v", redirect, err)
		}
		srv.Close()
	}
	if leaked.Load() != 0 {
		t.Fatalf("key sent %d times to proxy/redirect target", leaked.Load())
	}
}
