// Command labelprint is a one-page web app that prints uploaded ZPL, PDF, PNG
// and JPEG files on a label printer shared by LPrint over IPP.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type config struct {
	LPrintURI    string
	Listen       string
	MaxUpload    int64 // bytes
	MaxPages     int
	MaxCopies    int
	DefaultMedia string
	DPI          int
	UserHeader   string
}

func loadConfig() (config, error) {
	c := config{
		LPrintURI:    env("LPRINT_URI", ""),
		Listen:       env("LISTEN_ADDR", ":8080"),
		DefaultMedia: env("DEFAULT_MEDIA", "na_index-4x6_4x6in"),
		UserHeader:   env("USER_HEADER", ""), // opt-in: only trust a header your proxy sets
	}
	var errs []error
	// No default queue: there's no address that would be right for anyone else.
	if c.LPrintURI == "" {
		errs = append(errs, errors.New("LPRINT_URI is required, e.g. ipp://lprint-host:8000/ipp/print/<queue> "+
			"(the queue's URI as LPrint shows it)"))
	}
	mb := envInt("MAX_UPLOAD_MB", 20, 1, 200, &errs)
	c.MaxUpload = int64(mb) << 20
	c.MaxPages = envInt("MAX_PAGES", 50, 1, 500, &errs)
	c.MaxCopies = envInt("MAX_COPIES", 100, 1, 999, &errs)
	c.DPI = envInt("RENDER_DPI", 203, 72, 600, &errs)
	return c, errors.Join(errs...)
}

func env(key, dflt string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return dflt
}

func envInt(key string, dflt, lo, hi int, errs *[]error) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a number from %d to %d", key, v, lo, hi))
		return dflt
	}
	return n
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("bad configuration", "error", err)
		os.Exit(2)
	}

	// `labelprint healthcheck` is the image's HEALTHCHECK: the runtime image
	// has no curl, and the check should only prove this process serves HTTP.
	// LPrint being down is a printer problem, not a reason to restart us.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg.Listen))
	}

	ipp, err := newIPPClient(cfg.LPrintURI)
	if err != nil {
		log.Error("bad configuration", "error", err)
		os.Exit(2)
	}
	s := &server{cfg: cfg, ipp: ipp, pdf: newPDFRenderer(cfg.DPI, log), zpl: newZPLRenderer(cfg.DPI),
		shares: newShareStore(cfg.MaxUpload), log: log}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // a 20 MB upload over a slow link
		WriteTimeout:      5 * time.Minute, // rendering + sending a 50-page pdf on a pi
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go probeLPrint(ctx, log, ipp)
	go s.shares.janitor(ctx, time.Minute)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "lprint", cfg.LPrintURI)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		log.Error("server stopped", "error", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "error", err)
	}
}

// probeLPrint logs once at startup whether the queue is reachable — the first
// thing to check when the container's network can't see the host.
func probeLPrint(ctx context.Context, log *slog.Logger, ipp *ippClient) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := ipp.printerAttributes(ctx)
	if err != nil {
		log.Warn("lprint not reachable yet", "error", err)
		return
	}
	log.Info("lprint reachable", "state", info.State, "reasons", info.Reasons, "media_sizes", len(info.MediaSupported))
}

func healthcheck(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.Status)
		return 1
	}
	return 0
}
