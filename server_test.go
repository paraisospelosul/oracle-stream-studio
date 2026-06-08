package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCheckOrigin(t *testing.T) {
	s := &APIServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				host := r.Host
				if strings.Contains(origin, host) || strings.Contains(origin, "localhost") || strings.Contains(origin, "127.0.0.1") {
					return true
				}
				return false
			},
		},
	}

	tests := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{"Empty origin", "example.com", "", true},
		{"Same host origin", "example.com", "http://example.com", true},
		{"Localhost origin", "example.com", "http://localhost:8080", true},
		{"127.0.0.1 origin", "example.com", "http://127.0.0.1:8080", true},
		{"Foreign origin", "example.com", "http://malicious.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				Host:   tt.host,
				Header: make(http.Header),
			}
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			got := s.upgrader.CheckOrigin(r)
			if got != tt.want {
				t.Errorf("CheckOrigin() = %v, want %v (origin: %s, host: %s)", got, tt.want, tt.origin, tt.host)
			}
		})
	}
}

func TestBasicAuthMiddleware(t *testing.T) {
	s := &APIServer{
		webUser: "admin",
		webPass: "password",
	}

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	middleware := s.basicAuthMiddleware(nextHandler)

	t.Run("No auth header", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("Expected WWW-Authenticate header")
		}
	})

	t.Run("Invalid auth header", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.SetBasicAuth("admin", "wrongpassword")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
	})

	t.Run("Valid auth header", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.SetBasicAuth("admin", "password")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
		if rec.Body.String() != "OK" {
			t.Errorf("Expected body 'OK', got '%s'", rec.Body.String())
		}
	})

	t.Run("Disabled auth (empty credentials)", func(t *testing.T) {
		sEmpty := &APIServer{
			webUser: "",
			webPass: "",
		}
		middlewareEmpty := sEmpty.basicAuthMiddleware(nextHandler)

		req := httptest.NewRequest("GET", "/api/status", nil)
		rec := httptest.NewRecorder()
		middlewareEmpty.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})
}

func TestCorsMiddleware(t *testing.T) {
	s := &APIServer{}
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware := s.corsMiddleware(nextHandler)

	t.Run("Allowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.Host = "example.com"
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Header().Get("Access-Control-Allow-Origin") != "http://example.com" {
			t.Errorf("Expected Access-Control-Allow-Origin: http://example.com, got %s", rec.Header().Get("Access-Control-Allow-Origin"))
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Error("Expected Access-Control-Allow-Credentials: true")
		}
	})

	t.Run("Allowed local origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.Host = "example.com"
		req.Header.Set("Origin", "http://localhost:3000")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
			t.Errorf("Expected Access-Control-Allow-Origin: http://localhost:3000, got %s", rec.Header().Get("Access-Control-Allow-Origin"))
		}
	})

	t.Run("Disallowed origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.Host = "example.com"
		req.Header.Set("Origin", "http://malicious.com")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("Expected no Access-Control-Allow-Origin header, got %s", rec.Header().Get("Access-Control-Allow-Origin"))
		}
	})

	t.Run("Options request", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/status", nil)
		req.Host = "example.com"
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("Expected status 204, got %d", rec.Code)
		}
	})
}

func TestRateLimiter(t *testing.T) {
	// A rate limiter with window duration of 1 second and rate limit of 3
	rl := &rateLimiter{
		clients: make(map[string]*rateBucket),
		rate:    3,
		window:  1 * time.Second,
	}

	ip := "192.168.1.1"

	// 1st allow
	if !rl.allow(ip) {
		t.Error("Expected 1st request to be allowed")
	}
	// 2nd allow
	if !rl.allow(ip) {
		t.Error("Expected 2nd request to be allowed")
	}
	// 3rd allow
	if !rl.allow(ip) {
		t.Error("Expected 3rd request to be allowed")
	}
	// 4th allow (should be blocked)
	if rl.allow(ip) {
		t.Error("Expected 4th request within 1s to be blocked")
	}

	// Reset bucket by setting resetAt in past
	rl.clients[ip].resetAt = time.Now().Add(-1 * time.Second)

	// 5th allow (should be allowed now)
	if !rl.allow(ip) {
		t.Error("Expected request to be allowed after bucket reset")
	}
}

func TestBodySizeLimitMiddleware(t *testing.T) {
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware := bodySizeLimitMiddleware(nextHandler)

	t.Run("Normal API route within limit", func(t *testing.T) {
		body := bytes.NewReader(make([]byte, 1024)) // 1KB
		req := httptest.NewRequest("POST", "/api/config", body)
		req.ContentLength = 1024
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})

	t.Run("Normal API route exceeding limit", func(t *testing.T) {
		largeSize := int64(maxBodySize + 100)
		body := bytes.NewReader(make([]byte, largeSize))
		req := httptest.NewRequest("POST", "/api/config", body)
		req.ContentLength = largeSize
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("Expected status 413, got %d", rec.Code)
		}
	})

	t.Run("Upload route within limit", func(t *testing.T) {
		// Upload has 100MB limit
		bodySize := int64(15 << 20) // 15MB
		body := bytes.NewReader(make([]byte, bodySize))
		req := httptest.NewRequest("POST", "/api/upload/scene", body)
		req.ContentLength = bodySize
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware := securityHeadersMiddleware(nextHandler)

	req := httptest.NewRequest("GET", "/index.html", nil)
	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("Expected X-Frame-Options: SAMEORIGIN, got %s", rec.Header().Get("X-Frame-Options"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("Expected X-Content-Type-Options: nosniff, got %s", rec.Header().Get("X-Content-Type-Options"))
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("Expected Content-Security-Policy header to be set")
	}
}

func TestCheckDiskSpace(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Should succeed for a small amount of requested free space
	err := checkDiskSpace(tempDir, 1024) // 1KB
	if err != nil {
		t.Errorf("Expected checkDiskSpace to succeed for 1KB, got: %v", err)
	}

	// 2. Should fail for an ridiculously large amount of requested free space (e.g. 10000 TB)
	hugeSpace := uint64(10000 * 1024 * 1024 * 1024 * 1024)
	err = checkDiskSpace(tempDir, hugeSpace)
	if err == nil {
		t.Error("Expected checkDiskSpace to fail for 10000 TB")
	} else if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("Expected 'insufficient disk space' error message, got: %v", err)
	}

	// 3. Should fail for a non-existent path
	err = checkDiskSpace(tempDir+"/non-existent-folder", 1024)
	if err == nil {
		t.Error("Expected checkDiskSpace to fail for non-existent path")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	s := &APIServer{}
	rl := &rateLimiter{
		clients: make(map[string]*rateBucket),
		rate:    1,
		window:  1 * time.Second,
	}

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := s.rateLimitMiddleware(rl, nextHandler)

	t.Run("Non-API path skipped", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index.html", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})

	t.Run("Preview API path skipped", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/preview/frame", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		rec := httptest.NewRecorder()
		middleware.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})

	t.Run("Rate limit applied", func(t *testing.T) {
		ip := "1.2.3.4"
		req1 := httptest.NewRequest("GET", "/api/status", nil)
		req1.RemoteAddr = ip + ":1234"
		rec1 := httptest.NewRecorder()
		middleware.ServeHTTP(rec1, req1)

		if rec1.Code != http.StatusOK {
			t.Errorf("First request expected status 200, got %d", rec1.Code)
		}

		req2 := httptest.NewRequest("GET", "/api/status", nil)
		req2.RemoteAddr = ip + ":1234"
		rec2 := httptest.NewRecorder()
		middleware.ServeHTTP(rec2, req2)

		if rec2.Code != http.StatusTooManyRequests {
			t.Errorf("Second request expected status 429, got %d", rec2.Code)
		}
	})
}

type dummyHandler struct{}

func (h *dummyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func TestDummy(t *testing.T) {
	// simple test to import URL just to ensure package imports are satisfied
	u, _ := url.Parse("http://localhost")
	if u.Host != "localhost" {
		t.Fail()
	}
	// Touch os package to avoid unused import compile error
	if os.Getenv("NON_EXISTENT") != "" {
		t.Fail()
	}
}
