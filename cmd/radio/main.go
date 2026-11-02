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
	track       string
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

	track, err := initialTrack(ctx, cfg, lib)
	if err != nil {
		log.Fatalf("radio: подготовка трека: %v", err)
	}
	log.Printf("radio: в эфире %q (%s, %d мс)", track.Title, track.ID[:12], track.DurationMs)

	clk := clock.New()
	out := make(chan station.Outgoing, 256)
	st := station.New(clk, track, out)
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

// initialTrack — трек, который крутится в эфире до появления очереди (M3):
// файл из -track или сгенерированный метроном, через обычный конвейер загрузки.
func initialTrack(ctx context.Context, cfg config, lib *library.Library) (library.Track, error) {
	if cfg.track != "" {
		t, _, err := lib.Import(ctx, cfg.track, filepath.Base(cfg.track), "")
		return t, err
	}
	src := filepath.Join(lib.TmpDir(), "metronome.wav")
	defer os.Remove(src)
	if err := media.GenerateTestTrack(ctx, src, 180); err != nil {
		return library.Track{}, err
	}
	t, _, err := lib.Import(ctx, src, "Метроном.wav", "")
	return t, err
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
	flag.StringVar(&cfg.track, "track", envStr("RADIO_TRACK", ""), "до M3: аудиофайл для эфира (по умолчанию генерируется метроном)")
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
