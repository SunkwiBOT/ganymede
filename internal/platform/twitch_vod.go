package platform

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/zibbp/ganymede/internal/hls"
	"github.com/zibbp/ganymede/internal/utils"
)

const twitchVODProbeReadLimit = 2 * 1024 * 1024

type twitchVODQuality struct {
	Key        string
	Video      string
	Resolution string
	FrameRate  float64
	Bandwidth  int
}

var twitchVODQualities = []twitchVODQuality{
	{Key: "chunked", Video: "chunked", FrameRate: 60, Bandwidth: 8534030},
	{Key: "1440p60", Video: "1440p60", Resolution: "2560x1440", FrameRate: 60, Bandwidth: 8533930},
	{Key: "1080p60", Video: "1080p60", Resolution: "1920x1080", FrameRate: 60, Bandwidth: 8533830},
	{Key: "720p60", Video: "720p60", Resolution: "1280x720", FrameRate: 60, Bandwidth: 8533730},
	{Key: "480p30", Video: "480p30", Resolution: "854x480", FrameRate: 30, Bandwidth: 8533630},
	{Key: "360p30", Video: "360p30", Resolution: "640x360", FrameRate: 30, Bandwidth: 8533530},
	{Key: "160p30", Video: "160p30", Resolution: "284x160", FrameRate: 30, Bandwidth: 8533430},
	{Key: "audio_only", Video: "audio_only", Bandwidth: 160000},
}

func (c *TwitchConnection) getDirectTwitchVODPlayback(ctx context.Context, videoID string) (*hls.Multivariant, error) {
	gqlVideo, err := c.TwitchGQLGetVideo(videoID)
	if err != nil {
		return nil, fmt.Errorf("failed to get VOD metadata: %w", err)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var variants []*playlist.MultivariantVariant
	var lastErrors []string
	for _, quality := range twitchVODQualities {
		candidates, err := twitchVODPlaylistCandidates(videoID, gqlVideo, quality.Key)
		if err != nil {
			return nil, err
		}

		for _, candidate := range candidates {
			variant, err := fetchTwitchVODVariant(ctx, client, quality, candidate)
			if err != nil {
				lastErrors = append(lastErrors, fmt.Sprintf("%s %s: %v", quality.Key, candidate, err))
				continue
			}

			variants = append(variants, variant)
			break
		}
	}

	if len(variants) == 0 {
		errSuffix := ""
		if len(lastErrors) > 0 {
			start := len(lastErrors) - 4
			if start < 0 {
				start = 0
			}
			errSuffix = "; last errors: " + strings.Join(lastErrors[start:], " | ")
		}
		return nil, fmt.Errorf("no direct Twitch VOD playlists were valid%s", errSuffix)
	}

	return &playlist.Multivariant{
		Version:  3,
		Variants: variants,
	}, nil
}

func twitchVODPlaylistCandidates(videoID string, video *TwitchGQLVideo, quality string) ([]string, error) {
	seekPreviewsURL := strings.TrimSpace(video.SeekPreviewsURL)
	if seekPreviewsURL == "" {
		return nil, fmt.Errorf("video metadata did not include seekPreviewsURL")
	}

	parsed, err := url.Parse(seekPreviewsURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("invalid seekPreviewsURL: %s", seekPreviewsURL)
	}

	pathParts := splitURLPath(parsed.Path)
	storyboardIndex := -1
	for i, part := range pathParts {
		if strings.Contains(part, "storyboards") {
			storyboardIndex = i
			break
		}
	}
	if storyboardIndex <= 0 {
		return nil, fmt.Errorf("could not derive VOD storage id from seekPreviewsURL: %s", seekPreviewsURL)
	}

	storageID := pathParts[storyboardIndex-1]
	prefixParts := pathParts[:storyboardIndex-1]
	broadcastType := strings.ToLower(strings.TrimSpace(video.BroadcastType))
	ownerLogin := strings.TrimSpace(video.Owner.Login)

	var relativePaths [][]string
	switch broadcastType {
	case "highlight":
		relativePaths = append(relativePaths, []string{storageID, quality, "highlight-" + videoID + ".m3u8"})
	case "upload":
		if ownerLogin != "" {
			relativePaths = append(relativePaths, []string{ownerLogin, videoID, storageID, quality, "index-dvr.m3u8"})
		}
		relativePaths = append(relativePaths, []string{storageID, quality, "index-dvr.m3u8"})
	default:
		relativePaths = append(relativePaths, []string{storageID, quality, "index-dvr.m3u8"})
	}

	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}

	var candidates []string
	for _, relativePath := range relativePaths {
		candidates = appendUniqueString(candidates, buildTwitchVODURL(scheme, parsed.Host, relativePath))
		if len(prefixParts) > 0 {
			withPrefix := append(append([]string{}, prefixParts...), relativePath...)
			candidates = appendUniqueString(candidates, buildTwitchVODURL(scheme, parsed.Host, withPrefix))
		}
	}

	return candidates, nil
}

func splitURLPath(path string) []string {
	rawParts := strings.Split(path, "/")
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func buildTwitchVODURL(scheme string, host string, pathParts []string) string {
	return (&url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   "/" + strings.Join(pathParts, "/"),
	}).String()
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func fetchTwitchVODVariant(ctx context.Context, client *http.Client, quality twitchVODQuality, playlistURL string) (*playlist.MultivariantVariant, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playlistURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create playlist request: %w", err)
	}
	req.Header.Set("User-Agent", utils.ChromeUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch media playlist: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, twitchVODProbeReadLimit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read media playlist: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	playlistText := string(body)
	if !strings.Contains(playlistText, "#EXTM3U") {
		return nil, fmt.Errorf("response was not an HLS playlist")
	}

	codecs, err := detectTwitchVODCodecs(ctx, client, quality.Key, playlistURL, playlistText)
	if err != nil {
		return nil, err
	}

	variant := &playlist.MultivariantVariant{
		Bandwidth:  quality.Bandwidth,
		Codecs:     codecs,
		URI:        playlistURL,
		Resolution: quality.Resolution,
		Video:      quality.Video,
	}
	if quality.FrameRate > 0 {
		frameRate := quality.FrameRate
		variant.FrameRate = &frameRate
	}

	return variant, nil
}

func detectTwitchVODCodecs(ctx context.Context, client *http.Client, quality string, playlistURL string, playlistText string) ([]string, error) {
	if quality == "audio_only" {
		if strings.Contains(playlistText, "#EXTINF") || strings.Contains(playlistText, ".ts") || strings.Contains(playlistText, ".mp4") {
			return []string{"mp4a.40.2"}, nil
		}
		return nil, fmt.Errorf("audio playlist did not contain media segments")
	}

	if strings.Contains(playlistText, ".ts") {
		return []string{"avc1.4D001E", "mp4a.40.2"}, nil
	}

	if !strings.Contains(playlistText, ".mp4") {
		return nil, fmt.Errorf("playlist did not contain .ts or .mp4 segments")
	}

	videoCodec := "hev1.1.6.L93.B0"
	if initBytes, err := fetchTwitchVODInitSegment(ctx, client, playlistURL); err == nil && len(initBytes) > 0 {
		if bytes.Contains(initBytes, []byte("hev1")) || bytes.Contains(initBytes, []byte("hvc1")) {
			videoCodec = "hev1.1.6.L93.B0"
		} else {
			videoCodec = "avc1.4D001E"
		}
	}

	return []string{videoCodec, "mp4a.40.2"}, nil
}

func fetchTwitchVODInitSegment(ctx context.Context, client *http.Client, playlistURL string) ([]byte, error) {
	initURL := strings.Replace(playlistURL, "index-dvr.m3u8", "init-0.mp4", 1)
	if initURL == playlistURL {
		parsed, err := url.Parse(playlistURL)
		if err != nil {
			return nil, err
		}
		initURL = parsed.ResolveReference(&url.URL{Path: "init-0.mp4"}).String()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, initURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", utils.ChromeUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}
