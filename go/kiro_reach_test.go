package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Kiro's reachability check: any answer from either region's host is a
// network, a redirect included; only when neither answers is it none.
func TestKiroAnswersAnyRegionAndAnyStatus(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("method %s, want HEAD", r.Method)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("the check carried credentials")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer moved.Close()
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := gone.URL
	gone.Close()
	cases := []struct {
		name string
		urls []string
		want bool
	}{
		{"a 404 is a network", []string{notFound.URL}, true},
		{"a redirect is answered, not followed", []string{moved.URL}, true},
		{"one region down, the other up", []string{dead, notFound.URL}, true},
		{"nothing answers", []string{dead, dead}, false},
	}
	for _, c := range cases {
		if got := kiroAnswers(c.urls, 2*time.Second); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
