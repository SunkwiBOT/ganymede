package exec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/zibbp/ganymede/internal/hls"
)

func Test_extractSharedChatArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			name: "no shared flags",
			in:   []string{"-h", "1440", "-w", "340", "--font", "Inter"},
			want: nil,
		},
		{
			name: "equals form",
			in:   []string{"-h", "1440", "--stv=false", "--font", "Inter"},
			want: []string{"--stv=false"},
		},
		{
			name: "space form",
			in:   []string{"--bttv", "false", "-h", "1440"},
			want: []string{"--bttv", "false"},
		},
		{
			name: "all three providers mixed forms",
			in:   []string{"--framerate", "30", "--bttv=true", "--ffz", "false", "--stv=false"},
			want: []string{"--bttv=true", "--ffz", "false", "--stv=false"},
		},
		{
			name: "temp-path space form",
			in:   []string{"-h", "1440", "--temp-path", "/var/cache/td"},
			want: []string{"--temp-path", "/var/cache/td"},
		},
		{
			name: "temp-path equals form",
			in:   []string{"--temp-path=/var/cache/td", "--font", "Inter"},
			want: []string{"--temp-path=/var/cache/td"},
		},
		{
			name: "trailing flag without value",
			in:   []string{"--stv"},
			want: []string{"--stv"},
		},
		{
			name: "bare boolean does not swallow following flag",
			in:   []string{"--stv", "--temp-path", "/var/cache/td"},
			want: []string{"--stv", "--temp-path", "/var/cache/td"},
		},
		{
			name: "does not match prefix-only flags",
			in:   []string{"--stvthing", "--bttvfoo=1", "--temp-pathish"},
			want: nil,
		},
		{
			name: "collision is intentionally not forwarded",
			in:   []string{"--collision", "rename", "--stv=false"},
			want: []string{"--stv=false"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSharedChatArgs(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractSharedChatArgs(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func Test_appendFFmpegLiveOutputStreamArgs(t *testing.T) {
	tests := []struct {
		name      string
		audioOnly bool
		want      []string
	}{
		{
			name:      "all streams",
			audioOnly: false,
			want:      []string{"-map", "0", "-dn", "-ignore_unknown", "-c", "copy"},
		},
		{
			name:      "audio only",
			audioOnly: true,
			want:      []string{"-map", "0:a", "-dn", "-ignore_unknown", "-c", "copy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendFFmpegLiveOutputStreamArgs(nil, tt.audioOnly)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("appendFFmpegLiveOutputStreamArgs(nil, %t) = %v, want %v", tt.audioOnly, got, tt.want)
			}
		})
	}
}

func TestSelectTwitchVODPlaylistMapsSourceToChunked(t *testing.T) {
	masterPlaylist := &hls.Multivariant{
		Variants: []*playlist.MultivariantVariant{
			{Video: "chunked", URI: "https://cdn.example.com/chunked/index-dvr.m3u8"},
			{Video: "720p60", URI: "https://cdn.example.com/720p60/index-dvr.m3u8"},
		},
	}

	quality, uri, audioOnly, err := selectTwitchVODPlaylist(masterPlaylist, "source")
	if err != nil {
		t.Fatalf("selectTwitchVODPlaylist returned error: %v", err)
	}
	if quality != "chunked" {
		t.Fatalf("expected source to select chunked, got %q", quality)
	}
	if uri != "https://cdn.example.com/chunked/index-dvr.m3u8" {
		t.Fatalf("unexpected playlist URI: %q", uri)
	}
	if audioOnly {
		t.Fatal("source/chunked should not be audio-only")
	}
}

func TestSelectTwitchVODPlaylistMapsAudioToAudioOnly(t *testing.T) {
	masterPlaylist := &hls.Multivariant{
		Variants: []*playlist.MultivariantVariant{
			{Video: "chunked", URI: "https://cdn.example.com/chunked/index-dvr.m3u8"},
			{Video: "audio_only", URI: "https://cdn.example.com/audio_only/index-dvr.m3u8"},
		},
	}

	quality, uri, audioOnly, err := selectTwitchVODPlaylist(masterPlaylist, "audio")
	if err != nil {
		t.Fatalf("selectTwitchVODPlaylist returned error: %v", err)
	}
	if quality != "audio_only" {
		t.Fatalf("expected audio to select audio_only, got %q", quality)
	}
	if uri != "https://cdn.example.com/audio_only/index-dvr.m3u8" {
		t.Fatalf("unexpected playlist URI: %q", uri)
	}
	if !audioOnly {
		t.Fatal("audio should be audio-only")
	}
}

func TestRewriteTwitchVODMediaPlaylistAbsolutizesAndUsesMutedSegments(t *testing.T) {
	input := `#EXTM3U
#EXT-X-MAP:URI="init-unmuted.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="../keys/key-unmuted.key"
#EXTINF:10.0,
segment-unmuted-000.ts
`

	output, err := rewriteTwitchVODMediaPlaylist(input, "https://cdn.example.com/vods/storage/chunked/index-dvr.m3u8")
	if err != nil {
		t.Fatalf("rewriteTwitchVODMediaPlaylist returned error: %v", err)
	}

	if strings.Contains(output, "-unmuted") {
		t.Fatalf("expected unmuted segment names to be rewritten:\n%s", output)
	}

	expectedParts := []string{
		`#EXT-X-MAP:URI="https://cdn.example.com/vods/storage/chunked/init-muted.mp4"`,
		`#EXT-X-KEY:METHOD=AES-128,URI="https://cdn.example.com/vods/storage/keys/key-muted.key"`,
		"https://cdn.example.com/vods/storage/chunked/segment-muted-000.ts",
	}
	for _, expected := range expectedParts {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected rewritten playlist to contain %q, got:\n%s", expected, output)
		}
	}
	if !strings.HasSuffix(output, "\n") {
		t.Fatalf("expected rewritten playlist to end with newline, got:\n%s", output)
	}
}
