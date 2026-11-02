package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"radio-ugolok/internal/clock"
	"radio-ugolok/internal/httpapi"
	"radio-ugolok/internal/hub"
	"radio-ugolok/internal/library"
	"radio-ugolok/internal/media"
	"radio-ugolok/internal/station"
	"radio-ugolok/web"
)

// uploadJobs — сколько загрузок перекодируется одновременно.
const uploadJobs = 2

type config struct {
	addr        string
	dataDir     string
	maxUploadMB int
	bitrate     int
	queueLimit  int
	ephemeral   bool
}

func main() {
	cfg := parseFlags()

	if err := media.CheckTools(); err != nil {
		log.Fatalf("radio: %v — установите ffmpeg", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lib, err := library.Open(library.Options{
		DataDir:     cfg.dataDir,
		BitrateKbps: cfg.bitrate,
		MaxJobs:     uploadJobs,
	})
	if err != nil {
		log.Fatalf("radio: библиотека: %v", err)
	}
	defer lib.Close()
	cleanTmp(lib.TmpDir())

	clk := clock.New()
	out := make(chan station.Outgoing, 256)
	st := station.New(clk, lib, out, station.Options{
		QueueLimit: cfg.queueLimit,
		Ephemeral:  cfg.ephemeral,
	})
	h := hub.New(clk, st, out)

	api := &httpapi.Server{
		Station:   st,
		Library:   lib,
		WS:        h.ServeWS,
		Web:       web.FS,
		MaxUpload: int64(cfg.maxUploadMB) << 20,
	}
	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go st.Run(ctx)
	go h.Run(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("radio: слушаю %s", cfg.addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("radio: %v", err)
	}
	log.Print("radio: остановлен")
}

// cleanTmp убирает временные файлы, оставшиеся от прерванных загрузок.
func cleanTmp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", envStr("RADIO_ADDR", ":8080"), "адрес прослушивания")
	flag.StringVar(&cfg.dataDir, "data", envStr("RADIO_DATA", "./data"), "корень хранилища")
	flag.IntVar(&cfg.maxUploadMB, "max-upload", envInt("RADIO_MAX_UPLOAD", 150), "лимит загрузки, МБ")
	flag.IntVar(&cfg.bitrate, "bitrate", envInt("RADIO_BITRATE", 192), "битрейт эфирного mp3, kbps")
	flag.IntVar(&cfg.queueLimit, "queue-limit", envInt("RADIO_QUEUE_LIMIT", 3), "позиций в очереди на одного человека")
	flag.BoolVar(&cfg.ephemeral, "ephemeral", envBool("RADIO_EPHEMERAL", false), "удалять трек через 5 секунд после проигрывания")
	flag.Parse()
	return cfg
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "radio: %s=%q не число, беру %d\n", key, v, def)
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "radio: %s=%q не bool, беру %v\n", key, v, def)
		return def
	}
	return b
}
