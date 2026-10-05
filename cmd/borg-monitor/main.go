// borg-monitor: read-only web dashboard for Borg/borgmatic backups.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daschmidt1994/borg-backup-monitor/internal/borg"
	"github.com/daschmidt1994/borg-backup-monitor/internal/config"
	"github.com/daschmidt1994/borg-backup-monitor/internal/demo"
	"github.com/daschmidt1994/borg-backup-monitor/internal/monitor"
	"github.com/daschmidt1994/borg-backup-monitor/internal/restore"
	"github.com/daschmidt1994/borg-backup-monitor/internal/store"
	"github.com/daschmidt1994/borg-backup-monitor/internal/web"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("BBM_CONFIG", "/etc/borg-monitor/config.yaml"), "Konfigurationsdatei")
	demoMode := flag.Bool("demo", os.Getenv("BBM_DEMO") == "1", "Demo-Modus mit Beispieldaten (kein Zugriff auf echte Repositorys)")
	listen := flag.String("listen", os.Getenv("BBM_LISTEN"), "Adresse überschreiben, z. B. 0.0.0.0:8080")
	hash := flag.Bool("hash-password", false, "Passwort von der Standardeingabe lesen und bcrypt-Hash ausgeben")
	token := flag.Bool("new-token", false, "neuen ping_token ausgeben")
	check := flag.Bool("check-config", false, "Konfiguration prüfen und beenden")
	showVersion := flag.Bool("version", false, "Version ausgeben")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("borg-monitor", version)
		return
	case *token:
		fmt.Println(web.NewToken())
		return
	case *hash:
		fmt.Fprint(os.Stderr, "Passwort: ")
		pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(pw, "\r\n")
		if len(pw) < 10 {
			fmt.Fprintln(os.Stderr, "\nBitte mindestens 10 Zeichen.")
			os.Exit(1)
		}
		h, err := web.HashPassword(pw)
		if err != nil {
			fatal(err)
		}
		fmt.Println(h)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level()}))
	var (
		cfg     *config.Config
		st      *store.Store
		backend borg.Backend
		err     error
	)
	if *demoMode {
		dir, err := os.MkdirTemp("", "borg-monitor-demo-")
		if err != nil {
			fatal(err)
		}
		defer os.RemoveAll(dir)
		cfg = demo.Config(dir)
		st, _ = store.Open("")
		backend = demo.Seed(cfg, st, time.Now())
		log.Warn("DEMO-MODUS: Beispieldaten, keine echten Repositorys. Anmeldung: demo / demo")
	} else {
		cfg, err = config.Load(*cfgPath)
		if err != nil {
			fatal(err)
		}
		if *check {
			fmt.Printf("Konfiguration in Ordnung: %d Hosts, %d Jobs\n", len(cfg.Hosts), len(cfg.Jobs()))
			return
		}
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			fatal(err)
		}
		st, err = store.Open(filepath.Join(cfg.DataDir, "state.json"))
		if err != nil {
			fatal(fmt.Errorf("state.json: %w", err))
		}
		backend = borg.NewRunner(cfg)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	mon := monitor.New(cfg, st, backend, log)
	mon.Demo = *demoMode
	rm := restore.New(cfg, st, backend, mon)
	rm.Recover()
	srv, err := web.New(cfg, mon, rm, log, *demoMode)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go mon.Run(ctx)
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("borg-monitor startet", "version", version, "listen", cfg.Listen, "demo", *demoMode, "jobs", len(cfg.Jobs()))
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func level() slog.Level {
	if os.Getenv("BBM_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Fehler:", err)
	os.Exit(1)
}
