package fa

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// DefaultAddr is the listen address used by Serve when FA_ADDR is unset.
const DefaultAddr = "localhost:7373"

// Serve runs mux as an HTTP server and blocks until the process receives
// SIGINT or SIGTERM, then drains gracefully (Shutdown marks /readyz unhealthy,
// closes SSE connections) and stops the server with a 10s timeout.
//
// This is the entire server lifecycle in one call: app code never imports
// net/http, os/signal, or syscall, and main() stays inert. The address comes
// from FA_ADDR, defaulting to DefaultAddr. Requests are wrapped with
// LogRequests.
func (a *App) Serve(mux *http.ServeMux) error {
	addr := os.Getenv("FA_ADDR")
	if addr == "" {
		addr = DefaultAddr
	}
	srv := &http.Server{Addr: addr, Handler: LogRequests(mux)}

	errc := make(chan error, 1)
	go func() {
		log.Printf("fa: listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-stop:
	}

	log.Println("fa: shutting down…")
	a.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
