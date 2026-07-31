package main

// The dashboard, compiled in. One file, no CDN, no fonts fetched from
// anywhere: a relay someone runs for privacy must not have a page that
// phones a third party for its own chrome. What ships is what renders.

import (
	_ "embed"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML []byte

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Belt to the braces the page already wears (every dynamic string lands
	// via textContent): nothing loads from anywhere else, nothing frames it.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"img-src data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(dashboardHTML)
}
