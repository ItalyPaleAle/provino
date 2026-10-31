// Command example is a small application to try provino with: it signs users in, authorizes their uploads, receives provino's webhooks, and shows what provino stored
//
// It plays every part an application around provino plays, in one process and without any setup:
//   - A minimal OpenID Connect provider, with two accounts that sign in without a password, which issues the JWTs browsers present to provino and the client-credentials tokens provino presents to this app
//   - The authorizer provino asks before storing a browser upload, which picks the namespace, the formats, the size limit and the variants from hardcoded rules
//   - The webhook endpoint provino reports every job to
//   - A web UI that uploads straight to provino, follows the jobs, and shows each image's variants
//
// See example/README.md for how to run it next to provino
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Options of the app, which have to agree with example/provino.yaml
type options struct {
	// Address the app listens on
	Listen string
	// URL browsers and provino reach the app at, which is also the issuer of the tokens it signs
	PublicURL string
	// URL browsers reach provino at, which is also the audience of the tokens users present to it
	ProvinoURL string
	// Folder everything is stored in: the app's SQLite database, and provino's stores, which the app serves the variants from
	DataDir string
}

func main() {
	var opts options
	flag.StringVar(&opts.Listen, "listen", "localhost:3001", "Address to listen on")
	flag.StringVar(&opts.PublicURL, "url", "http://localhost:3001", "URL the app is reached at, which is the issuer of its tokens")
	flag.StringVar(&opts.ProvinoURL, "provino", "http://localhost:3000", "URL provino is reached at, which is the audience of the users' tokens")
	flag.StringVar(&opts.DataDir, "data", filepath.Join("example", "data"), "Folder the app and provino store their data in")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	err := run(log, opts)
	if err != nil {
		log.Error("The example app stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

// Start the app and serve it until the process is interrupted
func run(log *slog.Logger, opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(ctx, log, opts)
	if err != nil {
		return err
	}
	defer app.Close() //nolint:errcheck // The process is stopping, and SQLite has written everything already

	// The read timeouts only bound the small requests the app answers, since uploads go straight to provino
	srv := &http.Server{
		Addr:              opts.Listen,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	log.Info("The example app is running", slog.String("url", opts.PublicURL), slog.String("provino", opts.ProvinoURL), slog.String("data", opts.DataDir))

	// Stop on a signal, or when the server can't listen
	select {
	case err = <-errCh:
		return fmt.Errorf("failed to serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("failed to shut down: %w", err)
	}

	return nil
}

// Trim the trailing slash of a configured URL, so paths can be appended to it
func baseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return "", fmt.Errorf("'%s' is not an absolute URL", raw)
	}

	return strings.TrimRight(u.String(), "/"), nil
}
