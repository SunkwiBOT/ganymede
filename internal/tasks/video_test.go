package tasks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zibbp/ganymede/ent"
)

func TestValidateNonEmptyFile(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		err := validateNonEmptyFile(filepath.Join(t.TempDir(), "missing.mp4"), "test file")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.mp4")
		if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
			t.Fatalf("failed to create empty file: %v", err)
		}

		err := validateNonEmptyFile(path, "test file")
		if err == nil {
			t.Fatal("expected error for empty file")
		}
	})

	t.Run("non-empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ok.mp4")
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("failed to create non-empty file: %v", err)
		}

		err := validateNonEmptyFile(path, "test file")
		if err != nil {
			t.Fatalf("expected nil error for non-empty file, got: %v", err)
		}
	})
}

func TestValidateRecoverableLiveVideoInput(t *testing.T) {
	t.Run("missing input is not recoverable", func(t *testing.T) {
		dir := t.TempDir()
		video := &ent.Vod{
			TmpVideoDownloadPath: filepath.Join(dir, "missing.ts"),
		}

		if err := validateRecoverableLiveVideoInput(video); err == nil {
			t.Fatal("expected error for missing live video input")
		}
	})

	t.Run("non-empty transport stream is recoverable", func(t *testing.T) {
		dir := t.TempDir()
		tsPath := filepath.Join(dir, "video.ts")
		if err := os.WriteFile(tsPath, []byte("ts-data"), 0o644); err != nil {
			t.Fatalf("failed to create ts file: %v", err)
		}
		video := &ent.Vod{
			TmpVideoDownloadPath: tsPath,
		}

		if err := validateRecoverableLiveVideoInput(video); err != nil {
			t.Fatalf("expected recoverable ts input, got: %v", err)
		}
	})

	t.Run("non-empty converted mp4 is recoverable without source ts", func(t *testing.T) {
		dir := t.TempDir()
		mp4Path := filepath.Join(dir, "video.mp4")
		if err := os.WriteFile(mp4Path, []byte("mp4-data"), 0o644); err != nil {
			t.Fatalf("failed to create mp4 file: %v", err)
		}
		video := &ent.Vod{
			TmpVideoDownloadPath: filepath.Join(dir, "missing.ts"),
			TmpVideoConvertPath:  mp4Path,
		}

		if err := validateRecoverableLiveVideoInput(video); err != nil {
			t.Fatalf("expected recoverable converted input, got: %v", err)
		}
	})

	t.Run("empty converted mp4 with source ts is recoverable for retry", func(t *testing.T) {
		dir := t.TempDir()
		tsPath := filepath.Join(dir, "video.ts")
		mp4Path := filepath.Join(dir, "video.mp4")
		if err := os.WriteFile(tsPath, []byte("ts-data"), 0o644); err != nil {
			t.Fatalf("failed to create ts file: %v", err)
		}
		if err := os.WriteFile(mp4Path, []byte{}, 0o644); err != nil {
			t.Fatalf("failed to create empty mp4 file: %v", err)
		}
		video := &ent.Vod{
			TmpVideoDownloadPath: tsPath,
			TmpVideoConvertPath:  mp4Path,
		}

		if err := validateRecoverableLiveVideoInput(video); err != nil {
			t.Fatalf("expected source ts to make retry recoverable, got: %v", err)
		}
	})

	t.Run("empty converted mp4 without source ts is not recoverable", func(t *testing.T) {
		dir := t.TempDir()
		mp4Path := filepath.Join(dir, "video.mp4")
		if err := os.WriteFile(mp4Path, []byte{}, 0o644); err != nil {
			t.Fatalf("failed to create empty mp4 file: %v", err)
		}
		video := &ent.Vod{
			TmpVideoDownloadPath: filepath.Join(dir, "missing.ts"),
			TmpVideoConvertPath:  mp4Path,
		}

		if err := validateRecoverableLiveVideoInput(video); err == nil {
			t.Fatal("expected error for empty converted input without source ts")
		}
	})

	t.Run("non-empty hls playlist is recoverable", func(t *testing.T) {
		dir := t.TempDir()
		playlistPath := filepath.Join(dir, "123-video.m3u8")
		if err := os.WriteFile(playlistPath, []byte("#EXTM3U\n"), 0o644); err != nil {
			t.Fatalf("failed to create hls playlist: %v", err)
		}
		video := &ent.Vod{
			ExtID:           "123",
			VideoHlsPath:    filepath.Join(dir, "final-hls"),
			TmpVideoHlsPath: dir,
		}

		if err := validateRecoverableLiveVideoInput(video); err != nil {
			t.Fatalf("expected recoverable hls playlist, got: %v", err)
		}
	})
}
