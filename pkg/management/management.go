// Package management exposes a management server accessible via the internal network.
// The end-user can access this server securely and use it to configure, start, stop, and
// monitor the status of the network.
package management

import "net/http"

// Handler returns an HTTP handler which serves the management dashboard.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, world!"))
	})
	return mux
}
