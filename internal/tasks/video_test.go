package tasks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/riverqueue/river/rivertype"
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

func TestIsFinalJobAttempt(t *testing.T) {
	tests := []struct {
		name string
		job  *rivertype.JobRow
		want bool
	}{
		{
			name: "intermediate attempt",
			job:  &rivertype.JobRow{Attempt: 1, MaxAttempts: 3},
			want: false,
		},
		{
			name: "last attempt",
			job:  &rivertype.JobRow{Attempt: 3, MaxAttempts: 3},
			want: true,
		},
		{
			name: "attempt beyond limit",
			job:  &rivertype.JobRow{Attempt: 4, MaxAttempts: 3},
			want: true,
		},
		{
			name: "missing attempt limit is treated as final",
			job:  &rivertype.JobRow{Attempt: 1},
			want: true,
		},
		{
			name: "nil job is treated as final",
			job:  nil,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFinalJobAttempt(tt.job); got != tt.want {
				t.Fatalf("isFinalJobAttempt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsLiveArchiveJobStale(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-30 * time.Second)
	old := now.Add(-2 * time.Minute)

	tests := []struct {
		name string
		job  *rivertype.JobRow
		args RiverJobArgs
		want bool
	}{
		{
			name: "fresh custom heartbeat",
			job:  &rivertype.JobRow{CreatedAt: old},
			args: RiverJobArgs{Input: ArchiveVideoInput{HeartBeatTime: recent}},
			want: false,
		},
		{
			name: "expired custom heartbeat",
			job:  &rivertype.JobRow{CreatedAt: recent},
			args: RiverJobArgs{Input: ArchiveVideoInput{HeartBeatTime: old}},
			want: true,
		},
		{
			name: "missing heartbeat falls back to recent attempt",
			job:  &rivertype.JobRow{AttemptedAt: &recent, CreatedAt: old},
			want: false,
		},
		{
			name: "missing heartbeat falls back to old attempt",
			job:  &rivertype.JobRow{AttemptedAt: &old, CreatedAt: recent},
			want: true,
		},
		{
			name: "missing heartbeat and attempt falls back to creation",
			job:  &rivertype.JobRow{CreatedAt: old},
			want: true,
		},
		{
			name: "missing timestamps stays active conservatively",
			job:  &rivertype.JobRow{},
			want: false,
		},
		{
			name: "nil job is stale",
			job:  nil,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLiveArchiveJobStale(tt.job, tt.args, now); got != tt.want {
				t.Fatalf("isLiveArchiveJobStale() = %v, want %v", got, tt.want)
			}
		})
	}
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
