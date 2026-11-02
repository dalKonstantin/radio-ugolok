// Package media оборачивает вызовы ffmpeg/ffprobe.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// GenerateTestTrack пишет в dst метроном для проверки синхронизации на слух:
// щелчок каждую секунду, на каждой четвёртой — высокий тон. Формат эфирный:
// MP3 CBR, 44,1 кГц, стерео.
func GenerateTestTrack(ctx context.Context, dst string, seconds, bitrateKbps int) error {
	expr := "sin(2*PI*if(lt(mod(t,4),1),1760,880)*t)*exp(-25*mod(t,1))*0.6"
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("aevalsrc=exprs='%s':s=44100:d=%d", expr, seconds),
		"-ac", "2", "-ar", "44100", "-codec:a", "libmp3lame",
		"-b:a", fmt.Sprintf("%dk", bitrateKbps), "-write_xing", "1", "-f", "mp3", dst,
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
