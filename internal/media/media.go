// Package media оборачивает вызовы ffmpeg/ffprobe.
package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// CheckTools проверяет, что ffmpeg и ffprobe доступны в PATH.
func CheckTools() error {
	var missing []string
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("не найдены в PATH: %v", missing)
	}
	return nil
}

// DurationMs возвращает длительность файла по данным ffprobe.
func DurationMs(ctx context.Context, path string) (int64, error) {
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_entries", "format=duration", "-of", "json", path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	sec, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: длительность %q: %w", path, probe.Format.Duration, err)
	}
	return int64(sec * 1000), nil
}

// HashFile — sha256 содержимого файла в hex.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// GenerateTestTrack пишет в dst (wav) метроном для проверки синхронизации
// на слух: щелчок каждую секунду, на каждой четвёртой — высокий тон.
func GenerateTestTrack(ctx context.Context, dst string, seconds int) error {
	expr := "sin(2*PI*if(lt(mod(t,4),1),1760,880)*t)*exp(-25*mod(t,1))*0.6"
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("aevalsrc=exprs='%s':s=44100:d=%d", expr, seconds),
		"-ac", "2", "-f", "wav", dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return nil
}

// Transcode перекодирует src в эфирный формат: MP3 CBR, 44,1 кГц, стерео.
func Transcode(ctx context.Context, src, dst string, bitrateKbps int) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-vn", "-ac", "2", "-ar", "44100", "-codec:a", "libmp3lame",
		"-b:a", fmt.Sprintf("%dk", bitrateKbps), "-write_xing", "1", "-f", "mp3", dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return nil
}

type Stream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
}

type Probe struct {
	FormatName string
	Streams    []Stream
}

// ErrNotMedia — ffprobe не распознал файл как медиаконтейнер.
var ErrNotMedia = errors.New("не медиафайл")

// ProbeFile возвращает контейнер и потоки файла. Если ffprobe не смог
// разобрать файл, возвращает ErrNotMedia.
func ProbeFile(ctx context.Context, path string) (Probe, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_streams", "-show_format", "-of", "json", path,
	)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Probe{}, ErrNotMedia
		}
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}
	var raw struct {
		Streams []Stream `json:"streams"`
		Format  struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}
	return Probe{FormatName: raw.Format.FormatName, Streams: raw.Streams}, nil
}

// Cover уменьшает картинку до 500×500 с сохранением пропорций и пишет jpg.
func Cover(ctx context.Context, img []byte, dst string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "image2pipe", "-i", "-",
		"-vf", "scale='min(500,iw)':'min(500,ih)':force_original_aspect_ratio=decrease",
		"-frames:v", "1", "-q:v", "3", "-f", "image2", "-c:v", "mjpeg", dst,
	)
	cmd.Stdin = bytes.NewReader(img)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, out)
	}
	return nil
}

// PeaksCount — сколько корзин пиков хранится на трек.
const PeaksCount = 1000

const peaksRate = 8000

// Peaks считает пики waveform: моно-PCM 8 кГц делится на PeaksCount равных
// корзин, в каждой — максимум модуля, нормированный по всему треку в 0–255.
func Peaks(ctx context.Context, path string, durationMs int64) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-ac", "1", "-ar", strconv.Itoa(peaksRate), "-f", "s16le", "-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}

	// Число сэмплов известно по длительности; хвост сверх неё уходит в
	// последнюю корзину.
	total := durationMs * peaksRate / 1000
	if total < PeaksCount {
		total = PeaksCount
	}
	maxima := make([]int, PeaksCount)
	r := bufio.NewReaderSize(stdout, 64<<10)
	var buf [2]byte
	for i := int64(0); ; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			break
		}
		v := int(int16(binary.LittleEndian.Uint16(buf[:])))
		if v < 0 {
			v = -v
		}
		bin := min(i*PeaksCount/total, PeaksCount-1)
		if v > maxima[bin] {
			maxima[bin] = v
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, stderr.Bytes())
	}

	peak := 0
	for _, m := range maxima {
		peak = max(peak, m)
	}
	out := make([]byte, PeaksCount)
	if peak == 0 {
		return out, nil
	}
	for i, m := range maxima {
		out[i] = byte(m * 255 / peak)
	}
	return out, nil
}
