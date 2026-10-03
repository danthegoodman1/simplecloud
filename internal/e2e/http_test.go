//go:build integration

package e2e_test

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// httpCode fetches url, allowing for the wait while a sleeping sandbox resumes.
func httpCode(t *testing.T, url string, timeout time.Duration) int {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
