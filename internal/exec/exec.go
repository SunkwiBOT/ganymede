package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	osExec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/zibbp/ganymede/ent"
	"github.com/zibbp/ganymede/internal/config"
	"github.com/zibbp/ganymede/internal/errors"
	"github.com/zibbp/ganymede/internal/hls"
	"github.com/zibbp/ganymede/internal/livestream"
	"github.com/zibbp/ganymede/internal/platform"
	"github.com/zibbp/ganymede/internal/utils"
)

const (
	archiveShutdownTimeout           = 300 * time.Second
	maxTwitchVODMediaPlaylistSize    = 64 * 1024 * 1024
	twitchVODFFmpegProtocolWhitelist = "file,http,https,tcp,tls,crypto"
	archiveKillWaitTimeout           = 5 * time.Second
	// LiveArchiveProcessShutdownTimeout bounds the shutdown path used by a
	// remotely cancelled live capture before archive finalization can begin.
	LiveArchiveProcessShutdownTimeout = archiveShutdownTimeout + archiveKillWaitTimeout

	liveArchiveStallDefaultSeconds = 180
	liveArchiveStallMinimumSeconds = 30

	archiveProcessForwarder = `
forward_term() {
	trap '' TERM
	# SIGTERM the group so ffmpeg flushes; backstop KILLs a stuck capture.
	# A kill mid-flush can truncate the playlist; post-process rebuilds it.
	# Poll child_pid so the helper exits when the capture ends instead of
	# leaving a sleep process in the group; KILL only a still-running child.
	( i=0; while kill -0 "$child_pid" 2>/dev/null && [ "$i" -lt 60 ]; do sleep 1; i=$((i+1)); done; kill -0 "$child_pid" 2>/dev/null && kill -s KILL -- "-$$" 2>/dev/null ) &
	escalation_pid=$!
	kill -s TERM -- "-$$"
}

forward_usr1() {
	# Parent died: no worker left to flush, so KILL immediately for watchdog recovery.
	kill -s KILL -- "-$$" 2>/dev/null
}

trap 'forward_term' TERM
trap 'forward_usr1' USR1

"$@" &
child_pid=$!
wait "$child_pid"
status=$?

# A trapped signal interrupts wait; keep waiting until the child is reaped.
while kill -0 "$child_pid" 2>/dev/null; do
	wait "$child_pid"
	status=$?
done

# The helper survives the group SIGTERM by design, so KILL and reap it here.
if [ -n "${escalation_pid:-}" ]; then
	kill -s KILL "$escalation_pid" 2>/dev/null
	wait "$escalation_pid" 2>/dev/null
fi

exit $status
`
)

var hlsURIAttributePattern = regexp.MustCompile(`URI="([^"]+)"`)

// ErrLiveArchiveStalled indicates that a live FFmpeg capture stopped making media progress.
var ErrLiveArchiveStalled = stdErrors.New("live archive stalled")

func appendLiveArchiveProgressArgs(args []string, enabled bool) []string {
	if !enabled {
		return args
	}
	return append(args, "-stats_period", "5", "-progress", "pipe:3")
}

func normalizeLiveArchiveStallTimeout(seconds int) (time.Duration, bool) {
	if seconds < liveArchiveStallMinimumSeconds {
		return liveArchiveStallDefaultSeconds * time.Second, false
	}
	return time.Duration(seconds) * time.Second, true
}

func parseAdvancingFFmpegOutTime(line string, previous int64, havePrevious bool) (int64, bool) {
	key, value, ok := strings.Cut(line, "=")
	if !ok || key != "out_time_us" {
		return previous, false
	}
	outTime, err := strconv.ParseInt(value, 10, 64)
	if err != nil || outTime < 0 || (havePrevious && outTime == previous) {
		return previous, false
	}
	return outTime, true
}

func appendFFmpegLiveOutputStreamArgs(args []string, audioOnly bool) []string {
	streamMap := "0"
	if audioOnly {
		streamMap = "0:a"
	}

	return append(args,
		"-map", streamMap,
		"-dn",
		"-ignore_unknown",
		"-c", "copy",
	)
}

// buildLiveHlsCaptureFFmpegArgs returns ffmpeg args for live HLS (fmp4) capture.
func buildLiveHlsCaptureFFmpegArgs(inputURI, playlistPath, segmentPattern, initFilename string, audioOnly bool, configFfmpegArgs string, monitorStalls bool) []string {
	ffmpegArgs := []string{
		"-y",
		"-hide_banner",
	}
	ffmpegArgs = appendLiveArchiveProgressArgs(ffmpegArgs, monitorStalls)
	ffmpegArgs = append(ffmpegArgs,
		"-fflags", "+genpts+discardcorrupt",
		"-rw_timeout", "30000000",
		"-timeout", "30000000",
		// Resume interrupted HTTP reads without ending the live capture.
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_on_network_error", "1",
		"-reconnect_max_retries", "2",
		"-reconnect_delay_max", "5",
		"-reconnect_delay_total_max", "30",
		// HLS segment requests do not inherit the HTTP reconnect options.
		"-seg_max_retry", "2",
		"-i", inputURI,
	)
	ffmpegArgs = appendFFmpegLiveOutputStreamArgs(ffmpegArgs, audioOnly)

	// Append user-defined (global) params before outputs
	ffmpegArgs = append(ffmpegArgs, strings.Fields(configFfmpegArgs)...)

	return append(ffmpegArgs,
		"-start_number", "0",
		"-hls_time", "10",
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "append_list+independent_segments+temp_file",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", initFilename,
		// Twitch ADTS AAC needs this for the fmp4 muxer; no-op otherwise.
		"-bsf:a", "aac_adtstoasc",
		"-hls_segment_filename", segmentPattern,
		"-f", "hls",
		playlistPath,
	)
}

func selectTwitchVODPlaylist(masterPlaylist *hls.Multivariant, requestedQuality string) (string, string, bool, error) {
	qualities := make([]string, 0, len(masterPlaylist.Variants))
	qualitiesURI := make(map[string]string, len(masterPlaylist.Variants))
	for _, variant := range masterPlaylist.Variants {
		qualities = append(qualities, variant.Video)
		qualitiesURI[variant.Video] = variant.URI
	}

	closestQuality := utils.SelectClosestQuality(requestedQuality, qualities)
	switch closestQuality {
	case "audio":
		closestQuality = "audio_only"
	case "source":
		closestQuality = "chunked"
	}

	playlistURI, ok := qualitiesURI[closestQuality]
	if !ok || playlistURI == "" {
		return "", "", false, fmt.Errorf("selected Twitch VOD HLS quality %q has no playlist URI", closestQuality)
	}

	return closestQuality, playlistURI, closestQuality == "audio_only", nil
}

func materializeTwitchVODMediaPlaylist(ctx context.Context, playlistURI string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURI, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create media playlist request: %w", err)
	}
	req.Header.Set("User-Agent", utils.ChromeUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch media playlist: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTwitchVODMediaPlaylistSize+1))
	if err != nil {
		return "", fmt.Errorf("failed to read media playlist: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received media playlist status code %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) > maxTwitchVODMediaPlaylistSize {
		return "", fmt.Errorf("media playlist exceeds maximum size of %d bytes", maxTwitchVODMediaPlaylistSize)
	}

	rewritten, err := rewriteTwitchVODMediaPlaylist(string(body), playlistURI)
	if err != nil {
		return "", err
	}

	tmpFile, err := os.CreateTemp("", "ganymede-twitch-vod-*.m3u8")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary media playlist: %w", err)
	}
	tmpPath := tmpFile.Name()
	keepFile := false
	defer func() {
		if !keepFile {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.WriteString(rewritten); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("failed to write temporary media playlist: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temporary media playlist: %w", err)
	}

	keepFile = true
	return tmpPath, nil
}

func rewriteTwitchVODMediaPlaylist(playlistText string, playlistURI string) (string, error) {
	baseURL, err := url.Parse(playlistURI)
	if err != nil {
		return "", fmt.Errorf("invalid media playlist URI: %w", err)
	}

	playlistText = strings.ReplaceAll(playlistText, "-unmuted", "-muted")

	var b strings.Builder
	lines := strings.SplitAfter(playlistText, "\n")
	for _, line := range lines {
		lineWithoutNewline := strings.TrimSuffix(line, "\n")
		newline := line[len(lineWithoutNewline):]
		lineWithoutCR := strings.TrimSuffix(lineWithoutNewline, "\r")
		cr := lineWithoutNewline[len(lineWithoutCR):]
		stripped := strings.TrimSpace(lineWithoutCR)

		switch {
		case stripped == "":
			b.WriteString(line)
		case strings.HasPrefix(stripped, "#"):
			b.WriteString(rewriteHLSURIAttributes(lineWithoutCR, baseURL) + cr + newline)
		default:
			b.WriteString(resolveHLSURI(baseURL, stripped) + cr + newline)
		}
	}

	output := b.String()
	if output != "" && !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return output, nil
}

func rewriteHLSURIAttributes(line string, baseURL *url.URL) string {
	return hlsURIAttributePattern.ReplaceAllStringFunc(line, func(match string) string {
		parts := hlsURIAttributePattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		return `URI="` + resolveHLSURI(baseURL, parts[1]) + `"`
	})
}

func resolveHLSURI(baseURL *url.URL, value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	return baseURL.ResolveReference(parsed).String()
}

// DownloadTwitchVideo downloads a Twitch video.
func DownloadTwitchVideo(ctx context.Context, video ent.Vod) error {
	// Get video channel
	videoChannel := video.QueryChannel()
	channel, err := videoChannel.Only(ctx)
	if err != nil {
		return err
	}
	video.Edges.Channel = channel

	env := config.GetEnvConfig()

	// Open download log file
	logFilePath := fmt.Sprintf("%s/%s-video.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging output to %s", logFilePath)

	tc := &platform.TwitchConnection{}
	masterPlaylist, err := tc.GetVideoPlayback(ctx, video.ExtID)
	if err != nil {
		return fmt.Errorf("failed to get custom Twitch VOD HLS playlist: %w", err)
	}
	if len(masterPlaylist.Variants) == 0 {
		return fmt.Errorf("custom Twitch VOD HLS playlist has no variants")
	}

	closestQuality, playlistURI, audioOnly, err := selectTwitchVODPlaylist(masterPlaylist, video.Resolution)
	if err != nil {
		return err
	}
	localPlaylistPath, err := materializeTwitchVODMediaPlaylist(ctx, playlistURI)
	if err != nil {
		return fmt.Errorf("failed to materialize Twitch VOD media playlist: %w", err)
	}
	defer func() {
		if err := os.Remove(localPlaylistPath); err != nil && !os.IsNotExist(err) {
			log.Debug().Err(err).Str("path", localPlaylistPath).Msg("failed to remove temporary Twitch VOD media playlist")
		}
	}()

	streamMap := "0"
	if audioOnly {
		streamMap = "0:a"
	}

	ffmpegArgs := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "warning",
		"-stats",
		"-fflags", "+genpts",
		"-protocol_whitelist", twitchVODFFmpegProtocolWhitelist,
		"-i", localPlaylistPath,
		"-map", streamMap,
		"-dn",
		"-ignore_unknown",
		"-c", "copy",
		"-f", "mp4",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		video.TmpVideoDownloadPath,
	}

	cmd := osExec.Command("ffmpeg", ffmpegArgs...)

	log.Info().Str("requested_quality", video.Resolution).Str("selected_quality", closestQuality).Msg("running custom Twitch HLS VOD download")

	cmd.Stderr = file
	cmd.Stdout = file

	cmd.SysProcAttr = vodArchiveProcessAttributes()
	done, err := startArchiveCommand(cmd)
	if err != nil {
		return fmt.Errorf("error starting custom Twitch HLS VOD download: %w", err)
	}

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// A partial VOD download is retried from scratch, so stop the complete
		// forwarder/ffmpeg process group immediately.
		if cmd.Process != nil {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !stdErrors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("failed to kill Twitch VOD download process group: %v", err)
			}
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		if err != nil {
			log.Error().Err(err).Msg("error running custom Twitch HLS VOD download")
			return fmt.Errorf("error running custom Twitch HLS VOD download: %w", err)
		}
	}

	return nil
}

func DownloadTwitchLiveVideo(ctx context.Context, video ent.Vod, channel ent.Channel, startChat chan bool) error {
	video.Edges.Channel = &channel
	env := config.GetEnvConfig()
	monitorStalls := !env.DisableLiveArchiveStallCheck
	var stallTimeout time.Duration
	if monitorStalls {
		var validStallTimeout bool
		stallTimeout, validStallTimeout = normalizeLiveArchiveStallTimeout(env.LiveArchiveStallTimeoutSeconds)
		if !validStallTimeout {
			log.Warn().Int("configured_seconds", env.LiveArchiveStallTimeoutSeconds).Int("default_seconds", liveArchiveStallDefaultSeconds).Msg("live archive stall timeout must be at least 30 seconds; using default")
		}
	}

	// open video log file
	logFilePath := fmt.Sprintf("%s/%s-video.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()

	log.Debug().Str("video_id", video.ID.String()).Msgf("logging ffmpeg output to %s", logFilePath)

	streamSource, err := livestream.Resolve(ctx, channel.Name)
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}
	masterPlaylist := streamSource.Playlist

	qualities := make([]string, 0, len(masterPlaylist.Variants))
	qualitiesURI := make(map[string]string, len(masterPlaylist.Variants))
	for _, variant := range masterPlaylist.Variants {
		qualities = append(qualities, variant.Video)
		qualitiesURI[variant.Video] = variant.URI
	}
	log.Debug().Strs("available_qualities", qualities).Msg("available stream qualities")
	for b, a := range qualitiesURI {
		log.Debug().Str("quality", b).Str("quality_uri", a).Msg("quality uri")
	}

	closestQuality := utils.SelectClosestQuality(video.Resolution, qualities)
	log.Info().Str("requested_quality", video.Resolution).Msgf("selected closest quality %s", closestQuality)

	if closestQuality == "audio" {
		closestQuality = "audio_only"
	}

	audioOnly := closestQuality == "audio_only"

	// Live temp capture is HLS (fmp4); MP4 is exported later if configured.
	if err := utils.CreateDirectory(video.TmpVideoHlsPath); err != nil {
		return fmt.Errorf("error creating hls directory: %w", err)
	}
	// Capture files use the immutable stream ID; ExtID may later become the VOD ID.
	captureID := liveCaptureID(video)
	ffmpegArgs := buildLiveHlsCaptureFFmpegArgs(
		qualitiesURI[closestQuality],
		filepath.Join(video.TmpVideoHlsPath, captureID+"-video.m3u8"),
		filepath.Join(video.TmpVideoHlsPath, captureID+"_segment%06d.m4s"),
		fmt.Sprintf("%s_init.mp4", captureID),
		audioOnly,
		config.Get().Parameters.VideoConvert,
		monitorStalls,
	)

	// Run ffmpeg
	cmd := osExec.Command("ffmpeg", ffmpegArgs...)
	cmd.SysProcAttr = liveArchiveProcessAttributes()
	var progressReader, progressWriter *os.File
	if monitorStalls {
		progressReader, progressWriter, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("create ffmpeg progress pipe: %w", err)
		}
		defer func() {
			_ = progressReader.Close()
			_ = progressWriter.Close()
		}()
		cmd.ExtraFiles = []*os.File{progressWriter}
	}

	log.Debug().Str("channel", channel.Name).Str("cmd", strings.Join(cmd.Args, " ")).Msgf("running ffmpeg")

	// start chat download
	startChat <- true

	cmd.Stderr = file
	cmd.Stdout = file

	done, err := startArchiveCommand(cmd)
	if progressWriter != nil {
		_ = progressWriter.Close()
	}
	if err != nil {
		return fmt.Errorf("error starting ffmpeg: %w", err)
	}
	var progressUpdates <-chan int64
	if progressReader != nil {
		progressUpdates = ffmpegProgressUpdates(progressReader)
	}
	err = superviseLiveArchiveCommand(ctx, cmd, done, progressUpdates, stallTimeout)
	if stdErrors.Is(err, ErrLiveArchiveStalled) {
		log.Warn().Str("channel", channel.Name).Str("video_id", video.ID.String()).Dur("timeout", stallTimeout).Msg("live archive capture stopped after ffmpeg made no media progress")
	}
	return err
}

func ffmpegProgressUpdates(reader io.Reader) <-chan int64 {
	updates := make(chan int64, 1)
	go func() {
		defer close(updates)
		scanner := bufio.NewScanner(reader)
		var previous int64
		havePrevious := false
		for scanner.Scan() {
			outTime, advanced := parseAdvancingFFmpegOutTime(scanner.Text(), previous, havePrevious)
			if !advanced {
				continue
			}
			previous = outTime
			havePrevious = true
			select {
			case updates <- outTime:
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- outTime:
				default:
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Warn().Err(err).Msg("failed to read ffmpeg progress output")
		}
	}()
	return updates
}

func superviseLiveArchiveCommand(ctx context.Context, cmd *osExec.Cmd, done <-chan error, progressUpdates <-chan int64, stallTimeout time.Duration) error {
	var stallTimer *time.Timer
	var stallTimerC <-chan time.Time
	var lastOutTime int64
	if stallTimeout > 0 {
		stallTimer = time.NewTimer(stallTimeout)
		stallTimerC = stallTimer.C
		defer stallTimer.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			stopLiveArchiveCommand(cmd, done)
			return ctx.Err()
		case commandErr := <-done:
			if commandErr != nil {
				log.Error().Err(commandErr).Msg("error running ffmpeg")
				return fmt.Errorf("error running ffmpeg: %w", commandErr)
			}
			return nil
		case outTime, ok := <-progressUpdates:
			if !ok {
				progressUpdates = nil
				continue
			}
			lastOutTime = outTime
			if stallTimer != nil {
				resetTimer(stallTimer, stallTimeout)
			}
		case <-stallTimerC:
			if ctx.Err() != nil {
				stopLiveArchiveCommand(cmd, done)
				return ctx.Err()
			}
			select {
			case commandErr := <-done:
				if commandErr != nil {
					return fmt.Errorf("error running ffmpeg: %w", commandErr)
				}
				return nil
			default:
			}
			stopLiveArchiveCommand(cmd, done)
			return fmt.Errorf("%w: no ffmpeg media progress for %s (last out_time_us=%d)", ErrLiveArchiveStalled, stallTimeout, lastOutTime)
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func stopLiveArchiveCommand(cmd *osExec.Cmd, done <-chan error) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		log.Error().Err(err).Msg("failed to send SIGTERM to ffmpeg process")
	}
	select {
	case err := <-done:
		log.Debug().Err(err).Msg("live archive command stopped")
		return
	case <-time.After(archiveShutdownTimeout):
	}
	log.Warn().Msg("ffmpeg process did not exit after SIGTERM, sending SIGKILL")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		log.Error().Err(err).Msg("failed to send SIGKILL to ffmpeg process")
	}
	select {
	case err := <-done:
		log.Debug().Err(err).Msg("live archive command stopped")
	case <-time.After(archiveKillWaitTimeout):
	}
}

// startArchiveCommand launches a forwarding shim as the worker's direct child.
// Pdeathsig (SIGUSR1) KILLs the group on worker death; SIGTERM lets ffmpeg
// flush with a KILL backstop. The creating goroutine stays on its OS thread
// for Pdeathsig.
func startArchiveCommand(cmd *osExec.Cmd) (<-chan error, error) {
	targetPath := cmd.Path
	targetArgs := append([]string(nil), cmd.Args[1:]...)
	shellPath, err := osExec.LookPath("sh")
	if err != nil {
		return nil, fmt.Errorf("find archive process forwarder shell: %w", err)
	}
	cmd.Path = shellPath
	cmd.Args = append(
		[]string{shellPath, "-c", archiveProcessForwarder, "archive-process-forwarder", targetPath},
		targetArgs...,
	)

	started := make(chan error, 1)
	done := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		done <- cmd.Wait()
	}()

	if err := <-started; err != nil {
		return nil, err
	}
	return done, nil
}

func liveArchiveProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
		// USR1 KILLs on parent death; TERM allows ffmpeg to flush.
		Pdeathsig: syscall.SIGUSR1,
	}
}

func vodArchiveProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
		// Same parent-death KILL semantics as live captures.
		Pdeathsig: syscall.SIGUSR1,
	}
}

func ConvertVideoToHLS(ctx context.Context, video ent.Vod) error {
	env := config.GetEnvConfig()
	captureID := liveCaptureID(video)
	playlistPath := filepath.Join(video.TmpVideoHlsPath, captureID+"-video.m3u8")
	segmentPattern := filepath.Join(video.TmpVideoHlsPath, captureID+"_segment%06d.m4s")
	initFilename := fmt.Sprintf("%s_init.mp4", captureID)
	ffmpegArgs := []string{"-y", "-hide_banner", "-i", video.TmpVideoConvertPath, "-c", "copy", "-start_number", "0", "-hls_time", "10", "-hls_list_size", "0", "-hls_playlist_type", "event", "-hls_flags", "append_list+independent_segments", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", initFilename, "-hls_segment_filename", segmentPattern, "-f", "hls", playlistPath}

	// open log file
	logFilePath := fmt.Sprintf("%s/%s-video-convert.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()

	log.Debug().Str("video_id", video.ID.String()).Msgf("logging ffmpeg output to %s", logFilePath)

	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(ffmpegArgs, " ")).Msgf("running ffmpeg")

	cmd := osExec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)

	cmd.Stderr = file
	cmd.Stdout = file

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting ffmpeg: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// Context was cancelled, kill the process
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill ffmpeg process: %v", err)
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		// Command finished normally
		if err != nil {
			log.Error().Err(err).Msg("error running ffmpeg")
			return fmt.Errorf("error running ffmpeg: %w", err)
		}
	}

	return nil
}

func buildHlsToMp4FFmpegArgs(video ent.Vod, playlistPath, exportPath, configFfmpegArgs string, normalizeTimestamps bool) []string {
	args := []string{"-y", "-hide_banner", "-i", playlistPath, "-map", "0", "-dn", "-ignore_unknown", "-c", "copy", "-f", "mp4", "-movflags", "+faststart", "-metadata", "title=" + video.Title}
	args = append(args, strings.Fields(configFfmpegArgs)...)
	if normalizeTimestamps {
		// Repair start offsets without re-encoding.
		args = append(args, "-bsf", "setts=ts=TS-STARTDTS")
	}
	return append(args, exportPath)
}

// ExportHlsToMp4 creates an MP4 from the finalized live HLS playlist.
func ExportHlsToMp4(ctx context.Context, video ent.Vod, exportPath string) error {
	env := config.GetEnvConfig()
	playlistPath := filepath.Join(video.TmpVideoHlsPath, liveCaptureID(video)+"-video.m3u8")

	// Append to the convert log to preserve ffmpeg output.
	logFilePath := fmt.Sprintf("%s/%s-video-convert.log", env.LogsDir, video.ID.String())
	file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging ffmpeg MP4 export output to %s", logFilePath)

	// Export atomically via a sibling temp file so crashes leave no partial.
	tmpExportPath, commit, cleanup, err := tempExportTarget(exportPath)
	if err != nil {
		return err
	}
	defer cleanup()

	return exportWithTimestampRepair(ctx, video.ID.String(), tmpExportPath,
		func(normalizeTimestamps bool) error {
			args := buildHlsToMp4FFmpegArgs(video, playlistPath, tmpExportPath, config.Get().Parameters.VideoConvert, normalizeTimestamps)
			return runHlsToMp4FFmpeg(ctx, video, args, tmpExportPath, file)
		},
		commit,
		file,
	)
}

// tempExportTarget creates a sibling temp file with atomic commit/cleanup.
func tempExportTarget(dest string) (tmpPath string, commit func() error, cleanup func(), err error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create temporary export file: %w", err)
	}
	tmpPath = tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", nil, nil, fmt.Errorf("close temporary export file: %w", err)
	}

	cleanup = func() {
		_ = os.Remove(tmpPath)
	}
	commit = func() error {
		if err := os.Chmod(tmpPath, 0o644); err != nil {
			return fmt.Errorf("set exported media permissions: %w", err)
		}
		if err := os.Rename(tmpPath, dest); err != nil {
			return fmt.Errorf("publish exported media %s: %w", dest, err)
		}
		return nil
	}

	return tmpPath, commit, cleanup, nil
}

// liveCaptureID returns the immutable HLS file prefix. Capture files keep the
// stream-ID names while Vod.ExtID may later become the Twitch VOD ID.
func liveCaptureID(video ent.Vod) string {
	return hls.LiveCaptureID(video.ExtID, video.ExtStreamID, video.TmpVideoDownloadPath)
}

// EnsureLiveHlsPlaylist finalizes the capture playlist, rebuilding it from
// segments when a kill truncated it. Errors only with no recoverable media.
// extID must be the immutable capture ID (see liveCaptureID), not the mutable
// Vod.ExtID which may have become the Twitch VOD ID.
func EnsureLiveHlsPlaylist(ctx context.Context, tmpHlsPath, extID string) (string, error) {
	if tmpHlsPath == "" || extID == "" {
		return "", fmt.Errorf("empty live HLS playlist path")
	}
	playlistPath := filepath.Join(tmpHlsPath, extID+"-video.m3u8")
	if info, err := os.Stat(playlistPath); err == nil && info.Size() > 0 && info.Mode().IsRegular() {
		if err := hls.FinalizeMediaPlaylist(playlistPath); err != nil {
			return "", fmt.Errorf("failed to finalize live HLS playlist: %w", err)
		}
		return playlistPath, nil
	}
	if hls.HasRecoverableSegments(tmpHlsPath, extID) {
		log.Warn().Str("playlist", playlistPath).Msg("live HLS playlist missing or truncated; rebuilding from segments on disk")
		probeDir, err := os.MkdirTemp("", "ganymede-hls-probe-*")
		if err != nil {
			return "", fmt.Errorf("create segment probe directory: %w", err)
		}
		defer func() {
			_ = os.RemoveAll(probeDir)
		}()
		initPath := filepath.Join(tmpHlsPath, extID+"_init.mp4")
		probe := func(ctx context.Context, path string) (float64, error) {
			// fmp4 segments need the init file, so probe via a mini playlist.
			mini := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:60\n" +
				"#EXT-X-MEDIA-SEQUENCE:0\n" +
				fmt.Sprintf("#EXT-X-MAP:URI=%q\n", hls.URI(initPath)) +
				"#EXTINF:60.0,\n" + hls.URI(path) + "\n#EXT-X-ENDLIST\n"
			miniPath := filepath.Join(probeDir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".m3u8")
			if err := os.WriteFile(miniPath, []byte(mini), 0o644); err != nil {
				return 0, err
			}
			return probeSegmentPacketDuration(ctx, miniPath)
		}
		if err := hls.RebuildMediaPlaylistFromSegments(ctx, tmpHlsPath, extID, playlistPath, probe); err != nil {
			return "", err
		}
		return playlistPath, nil
	}
	return "", fmt.Errorf("empty live HLS playlist: %s", playlistPath)
}

type segmentPacketProbe struct {
	Packets []struct {
		PtsTime     string `json:"pts_time"`
		StreamIndex int    `json:"stream_index"`
	} `json:"packets"`
}

// probeSegmentPacketDuration measures duration from packet pts, not container
// metadata which would echo the mini playlist placeholder values.
func probeSegmentPacketDuration(ctx context.Context, miniPlaylistPath string) (float64, error) {
	cmd := osExec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "packet=pts_time,stream_index",
		"-of", "json",
		miniPlaylistPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("error running ffprobe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var probe segmentPacketProbe
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("error parsing ffprobe output: %w", err)
	}

	type bounds struct {
		min, max float64
		seen     bool
	}
	perStream := make(map[int]*bounds)
	for _, packet := range probe.Packets {
		pts, err := strconv.ParseFloat(packet.PtsTime, 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) {
			continue
		}
		b, ok := perStream[packet.StreamIndex]
		if !ok {
			b = &bounds{min: pts, max: pts, seen: true}
			perStream[packet.StreamIndex] = b
			continue
		}
		b.min = math.Min(b.min, pts)
		b.max = math.Max(b.max, pts)
	}

	duration := 0.0
	for _, b := range perStream {
		duration = math.Max(duration, b.max-b.min)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("no packet timestamps in %s", miniPlaylistPath)
	}
	return duration, nil
}

func runHlsToMp4FFmpeg(ctx context.Context, video ent.Vod, ffmpegArgs []string, exportPath string, output io.Writer) error {
	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(ffmpegArgs, " ")).Msg("running ffmpeg")

	cmd := osExec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	cmd.Stderr = output
	cmd.Stdout = output

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting ffmpeg: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if err := cmd.Process.Kill(); err != nil {
			log.Error().Err(err).Msg("failed to kill ffmpeg process")
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		// Remove partial output so retries do not accept a truncated file.
		_ = os.Remove(exportPath)
		return ctx.Err()
	case err := <-done:
		if err != nil {
			// Remove partial output so retries do not accept a truncated file.
			_ = os.Remove(exportPath)
			log.Error().Err(err).Msg("error running ffmpeg")
			return fmt.Errorf("error running ffmpeg: %w", err)
		}
	}

	return nil
}

func PostProcessVideo(ctx context.Context, video ent.Vod) error {
	env := config.GetEnvConfig()
	configFfmpegArgs := config.Get().Parameters.VideoConvert

	// open log file
	logFilePath := fmt.Sprintf("%s/%s-video-convert.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging ffmpeg output to %s", logFilePath)

	return postProcessVideo(ctx, video, configFfmpegArgs, file)
}

// exportWithTimestampRepair runs the mux, probes the result, and when the
// container timeline is inflated re-runs the mux with per-stream timestamp
// normalization before committing. Both mux passes write to the same atomic
// temp target; commit publishes it.
func exportWithTimestampRepair(
	ctx context.Context,
	videoID string,
	exportPath string,
	runMux func(normalizeTimestamps bool) error,
	commit func() error,
	output io.Writer,
) error {
	if err := runMux(false); err != nil {
		return err
	}

	duration, err := ProbeMediaDuration(ctx, exportPath)
	if err != nil {
		return fmt.Errorf("probe exported video duration: %w", err)
	}
	if !duration.HasTimestampAnomaly() {
		return commit()
	}

	log.Warn().
		Str("video_id", videoID).
		Float64("format_duration", duration.FormatDuration).
		Float64("stream_duration", duration.LongestStreamDuration).
		Msg("detected anomalous container timestamps; normalizing each stream")

	if err := runMux(true); err != nil {
		return fmt.Errorf("normalize exported video timestamps: %w", err)
	}

	duration, err = ProbeMediaDuration(ctx, exportPath)
	if err != nil {
		return fmt.Errorf("probe timestamp-normalized video duration: %w", err)
	}
	if duration.HasTimestampAnomaly() {
		return fmt.Errorf(
			"timestamp normalization did not repair container duration: format=%f stream=%f",
			duration.FormatDuration,
			duration.LongestStreamDuration,
		)
	}

	return commit()
}

func postProcessVideo(ctx context.Context, video ent.Vod, configFfmpegArgs string, output io.Writer) error {
	// Write atomically via a sibling temp file for crash-safe retries.
	tmpExportPath, commit, cleanup, err := tempExportTarget(video.TmpVideoConvertPath)
	if err != nil {
		return err
	}
	defer cleanup()

	exportVideo := video
	exportVideo.TmpVideoConvertPath = tmpExportPath

	return exportWithTimestampRepair(ctx, video.ID.String(), tmpExportPath,
		func(normalizeTimestamps bool) error {
			args := buildPostProcessVideoFFmpegArgs(exportVideo, configFfmpegArgs, normalizeTimestamps)
			return runPostProcessVideoFFmpeg(ctx, exportVideo, args, output)
		},
		commit,
		output,
	)
}

func runPostProcessVideoFFmpeg(ctx context.Context, video ent.Vod, ffmpegArgs []string, output io.Writer) error {
	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(ffmpegArgs, " ")).Msg("running ffmpeg")

	cmd := osExec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	cmd.Stderr = output
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Error().Err(err).Msg("error running ffmpeg")
		return fmt.Errorf("error running ffmpeg: %w", err)
	}
	return nil
}

func postProcessVideoFFmpegArgs(video ent.Vod, configFfmpegArgs string) []string {
	return buildPostProcessVideoFFmpegArgs(video, configFfmpegArgs, false)
}

func normalizedPostProcessVideoFFmpegArgs(video ent.Vod, configFfmpegArgs string) []string {
	return buildPostProcessVideoFFmpegArgs(video, configFfmpegArgs, true)
}

func buildPostProcessVideoFFmpegArgs(video ent.Vod, configFfmpegArgs string, normalizeTimestamps bool) []string {
	arr := strings.Fields(configFfmpegArgs)
	ffmpegArgs := []string{"-y", "-hide_banner", "-fflags", "+genpts", "-i", video.TmpVideoDownloadPath, "-map", "0", "-dn", "-ignore_unknown", "-c", "copy", "-f", "mp4"}
	if !normalizeTimestamps {
		ffmpegArgs = append(ffmpegArgs, "-bsf:a", "aac_adtstoasc")
	}
	ffmpegArgs = append(ffmpegArgs, "-movflags", "+faststart")

	ffmpegArgs = append(ffmpegArgs, "-metadata", "title="+video.Title)
	ffmpegArgs = append(ffmpegArgs, arr...)
	if normalizeTimestamps {
		// Apply one bitstream-filter instance per output stream. STARTDTS is
		// therefore local to each stream, repairing large audio/video start
		// offsets without decoding or re-encoding the media.
		ffmpegArgs = append(ffmpegArgs, "-bsf", "setts=ts=TS-STARTDTS")
	}
	ffmpegArgs = append(ffmpegArgs, video.TmpVideoConvertPath)

	return ffmpegArgs
}

func DownloadTwitchChat(ctx context.Context, video ent.Vod) error {
	env := config.GetEnvConfig()
	// open log file
	logFilePath := fmt.Sprintf("%s/%s-chat.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging chatdownload output to %s", logFilePath)

	var cmdArgs []string
	cmdArgs = append(cmdArgs, "chatdownload", "--id", video.ExtID, "--embed-images", "--collision", "overwrite")

	// Forward shared chat flags from the chat render config so the user's
	// --bttv/--ffz/--stv/--temp-path preferences also apply to chatdownload,
	// which fetches emotes for embedding when --embed-images is set.
	configRenderArgs := config.Get().Parameters.ChatRender
	cmdArgs = append(cmdArgs, extractSharedChatArgs(strings.Fields(configRenderArgs))...)

	cmdArgs = append(cmdArgs, "-o", video.TmpChatDownloadPath)

	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(cmdArgs, " ")).Msgf("running TwitchDownloaderCLI")

	cmd := osExec.CommandContext(ctx, "TwitchDownloaderCLI", cmdArgs...)

	cmd.Stderr = file
	cmd.Stdout = file

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting TwitchDownloader: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// Context was cancelled, kill the process
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill TwitchDownloaderCLI process: %v", err)
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		// Command finished normally
		if err != nil {
			if exitError, ok := err.(*osExec.ExitError); ok {
				log.Error().Err(err).Msg("error running TwitchDownloaderCLI")
				return fmt.Errorf("error running TwitchDownloaderCLI exit code %d: %w", exitError.ExitCode(), exitError)
			}
			log.Error().Err(err).Msg("error running TwitchDownloaderCLI")
			return fmt.Errorf("error running TwitchDownloaderCLI: %w", err)
		}
	}

	return nil
}

func RenderTwitchChat(ctx context.Context, video ent.Vod) error {
	env := config.GetEnvConfig()
	// open log file
	logFilePath := fmt.Sprintf("%s/%s-chat-render.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging TwitchDownloaderCLI output to %s", logFilePath)

	var cmdArgs []string

	configRenderArgs := config.Get().Parameters.ChatRender
	configRenderArgsArr := strings.Fields(configRenderArgs)

	cmdArgs = append(cmdArgs, "chatrender", "-i", video.TmpChatDownloadPath, "--collision", "overwrite")

	cmdArgs = append(cmdArgs, configRenderArgsArr...)
	cmdArgs = append(cmdArgs, "-o", video.TmpChatRenderPath)

	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(cmdArgs, " ")).Msgf("running TwitchDownloaderCLI")

	cmd := osExec.CommandContext(ctx, "TwitchDownloaderCLI", cmdArgs...)

	cmd.Stderr = file
	cmd.Stdout = file

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting TwitchDownloader: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// Context was cancelled, kill the process
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill TwitchDownloaderCLI process: %v", err)
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		// Command finished normally
		if err != nil {
			if exitError, ok := err.(*osExec.ExitError); ok {
				log.Error().Err(err).Msg("error running TwitchDownloaderCLI")
				return fmt.Errorf("error running TwitchDownloaderCLI exit code %d: %w", exitError.ExitCode(), exitError)
			}

			// Check if log output indicates no messages
			noElements, err := checkLogForNoElements(logFilePath)
			if err == nil && noElements {
				return errors.ErrNoChatMessages
			}

			log.Error().Err(err).Msg("error running TwitchDownloaderCLI")
			return fmt.Errorf("error running TwitchDownloaderCLI: %w", err)
		}
	}

	return nil
}

// extractSharedChatArgs returns the subset of args from a chatrender arg list
// that are also valid for chatupdate and chatdownload.
func extractSharedChatArgs(args []string) []string {
	// --collision is omitted: every caller hardcodes it.
	sharedFlagNames := []string{"--bttv", "--ffz", "--stv", "--temp-path"}
	var result []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		for _, flag := range sharedFlagNames {
			if arg == flag {
				result = append(result, arg)
				// Only consume the next token as a value if it isn't itself a flag,
				// otherwise `--stv --temp-path /p` would swallow `--temp-path`.
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
					result = append(result, args[i+1])
					i++
				}
				break
			}
			if strings.HasPrefix(arg, flag+"=") {
				result = append(result, arg)
				break
			}
		}
	}
	return result
}

// checkLogForNoElements returns true if the log file contains the expected message.
//
// Used to check if the chat render failure was caused by no messages in the chat.
func checkLogForNoElements(logFilePath string) (bool, error) {
	file, err := os.Open(logFilePath)
	if err != nil {
		return false, fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "Sequence contains no elements") {
			return true, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("error reading log file: %w", err)
	}

	return false, nil
}

func UpdateTwitchChat(ctx context.Context, video ent.Vod) error {
	env := config.GetEnvConfig()
	// open log file
	logFilePath := fmt.Sprintf("%s/%s-chat-convert.log", env.LogsDir, video.ID.String())
	file, err := os.Create(logFilePath)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log file")
		}
	}()
	log.Debug().Str("video_id", video.ID.String()).Msgf("logging TwitchDownloader output to %s", logFilePath)

	var cmdArgs []string
	cmdArgs = append(cmdArgs, "chatupdate", "-i", video.TmpLiveChatConvertPath, "--embed-missing", "--collision", "overwrite")

	// Forward shared chat flags from the chat render config so the user's
	// --bttv/--ffz/--stv/--temp-path preferences also apply to chatupdate,
	// which fetches emotes for embedding and can otherwise hang on a
	// third-party timeout.
	configRenderArgs := config.Get().Parameters.ChatRender
	cmdArgs = append(cmdArgs, extractSharedChatArgs(strings.Fields(configRenderArgs))...)

	cmdArgs = append(cmdArgs, "-o", video.TmpChatDownloadPath)

	log.Debug().Str("video_id", video.ID.String()).Str("cmd", strings.Join(cmdArgs, " ")).Msgf("running TwitchDownloaderCLI")

	cmd := osExec.CommandContext(ctx, "TwitchDownloaderCLI", cmdArgs...)

	cmd.Stderr = file
	cmd.Stdout = file

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting TwitchDownloader: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// Context was cancelled, kill the process
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill TwitchDownloader process: %v", err)
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		// Command finished normally
		if err != nil {
			if exitError, ok := err.(*osExec.ExitError); ok {
				log.Error().Err(err).Str("exitCode", strconv.Itoa(exitError.ExitCode())).Str("exit_error", exitError.Error()).Msg("error running TwitchDownloader")
				return fmt.Errorf("error running TwitchDownloader")
			}
			return fmt.Errorf("error running TwitchDownloader: %w", err)
		}
	}

	return nil
}

// GenerateStaticThumbnail generates static thumbnail for video.
//
// Resolution is optional and if not set the thumbnail will be generated at the original resolution.
func GenerateStaticThumbnail(ctx context.Context, videoPath string, position int, thumbnailPath string, resolution string) error {
	log.Info().Str("videoPath", videoPath).Str("position", strconv.Itoa(position)).Str("thumbnailPath", thumbnailPath).Str("resolution", resolution).Msg("generating static thumbnail")
	// placing -ss 1 before the input is faster
	// https://stackoverflow.com/questions/27568254/how-to-extract-1-screenshot-for-a-video-with-ffmpeg-at-a-given-time
	ffmpegArgs := []string{"-y", "-hide_banner", "-ss", strconv.Itoa(position), "-i", videoPath, "-vframes", "1", "-update", "1"}
	if resolution != "" {
		ffmpegArgs = append(ffmpegArgs, "-s", resolution)
	}

	ffmpegArgs = append(ffmpegArgs, thumbnailPath)

	cmd := osExec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting ffmpeg: %w", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for the command to finish or context to be cancelled
	select {
	case <-ctx.Done():
		// Context was cancelled, kill the process
		if err := cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to kill ffmpeg process: %v", err)
		}
		log.Debug().Err(<-done).Msg("command exited after cancellation")
		return ctx.Err()
	case err := <-done:
		// Command finished normally
		if err != nil {
			log.Error().Err(err).Str("ffmpeg_stderr", stderr.String()).Str("ffmpeg_stdout", stdout.String()).Msg("error running ffmpeg")
			return fmt.Errorf("error running ffmpeg: %w", err)
		}
	}

	return nil
}
