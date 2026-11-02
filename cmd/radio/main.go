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
	"strings"
	"syscall"
	"time"

	"radio-ugolok/internal/clock"
	"radio-ugolok/internal/httpapi"
	"radio-ugolok/internal/hub"
	"radio-ugolok/internal/media"
	"radio-ugolok/internal/station"
	"radio-ugolok/web"
)

type config struct {
	addr    string
	dataDir string
	bitrate int
	track   string
}

func main() {
	cfg := parseFlags()

	if err := media.CheckTools(); err != nil {
		log.Fatalf("radio: %v — установите ffmpeg", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracksDir := filepath.Join(cfg.dataDir, "tracks")
	if err := os.MkdirAll(tracksDir, 0o755); err != nil {
		log.Fatalf("radio: %v", err)
	}

	track, err := prepareTrack(ctx, cfg, tracksDir)
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
		WS:        h.ServeWS,
		Web:       web.FS,
		TracksDir: tracksDir,
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

// prepareTrack готовит единственный трек M1: перекодирует файл из -track
// в эфирный формат или, если флаг не задан, генерирует метроном.
func prepareTrack(ctx context.Context, cfg config, tracksDir string) (station.Track, error) {
	var (
		id    string
		title string
		dst   string
	)
	if cfg.track != "" {
		hash, err := media.HashFile(cfg.track)
		if err != nil {
			return station.Track{}, err
		}
		id = hash
		title = strings.TrimSuffix(filepath.Base(cfg.track), filepath.Ext(cfg.track))
		dst = filepath.Join(tracksDir, id+".mp3")
		if _, err := os.Stat(dst); err != nil {
			tmp := dst + ".tmp"
			if err := media.Transcode(ctx, cfg.track, tmp, cfg.bitrate); err != nil {
				os.Remove(tmp)
				return station.Track{}, err
			}
			if err := os.Rename(tmp, dst); err != nil {
				return station.Track{}, err
			}
		}
	} else {
		tmp := filepath.Join(tracksDir, "m1-metronome.tmp")
		if err := media.GenerateTestTrack(ctx, tmp, 180, cfg.bitrate); err != nil {
			os.Remove(tmp)
			return station.Track{}, err
		}
		hash, err := media.HashFile(tmp)
		if err != nil {
			return station.Track{}, err
		}
		id = hash
		title = "Метроном M1"
		dst = filepath.Join(tracksDir, id+".mp3")
		if err := os.Rename(tmp, dst); err != nil {
			return station.Track{}, err
		}
	}

	dur, err := media.DurationMs(ctx, dst)
	if err != nil {
		return station.Track{}, err
	}
	return station.Track{ID: id, Title: title, Artist: "Radio Ugolok", DurationMs: dur}, nil
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", envStr("RADIO_ADDR", ":8080"), "адрес прослушивания")
	flag.StringVar(&cfg.dataDir, "data", envStr("RADIO_DATA", "./data"), "корень хранилища")
	flag.IntVar(&cfg.bitrate, "bitrate", envInt("RADIO_BITRATE", 192), "битрейт эфирного mp3, kbps")
	flag.StringVar(&cfg.track, "track", envStr("RADIO_TRACK", ""), "M1: аудиофайл для эфира (по умолчанию генерируется метроном)")
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
