package livestream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zibbp/ganymede/internal/hls"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestManagerRelaysPlaylistsAndSegmentsWithConfiguredHeaders(t *testing.T) {
	t.Parallel()

	master, err := hls.DecodeMultivariant(strings.NewReader(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.42e01e,mp4a.40.2",RESOLUTION=1280x720,VIDEO="720p"
https://media.example/quality.m3u8
`))
	if err != nil {
		t.Fatalf("decode master playlist: %v", err)
	}

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("X-Playback-Test"); got != "configured-header" {
			t.Fatalf("configured header = %q, want %q", got, "configured-header")
		}

		var body string
		var contentType string
		switch req.URL.Path {
		case "/quality.m3u8":
			body = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-KEY:METHOD=AES-128,URI="/key"
#EXTINF:2.0,
segment.ts
`
			contentType = "application/vnd.apple.mpegurl"
		case "/segment.ts":
			body = "video-segment"
			contentType = "video/mp2t"
		case "/key":
			body = "encryption-key"
			contentType = "application/octet-stream"
		default:
			t.Fatalf("unexpected upstream path %q", req.URL.Path)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{contentType},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: req,
		}, nil
	})

	headers := make(http.Header)
	headers.Set("X-Playback-Test", "configured-header")
	manager := newManager(func(context.Context, string) (*Source, error) {
		return &Source{
			Playlist: master,
			Client:   &http.Client{Transport: transport},
			Headers:  headers,
			Kind:     "test",
		}, nil
	}, time.Hour)

	sessionID, err := manager.Start(context.Background(), "example")
	if err != nil {
		t.Fatalf("start playback session: %v", err)
	}

	rewrittenMaster, err := manager.Master(sessionID)
	if err != nil {
		t.Fatalf("get rewritten master: %v", err)
	}
	parsedMaster, err := hls.DecodeMultivariant(strings.NewReader(string(rewrittenMaster)))
	if err != nil {
		t.Fatalf("decode rewritten master: %v", err)
	}
	if len(parsedMaster.Variants) != 1 {
		t.Fatalf("variant count = %d, want 1", len(parsedMaster.Variants))
	}

	mediaResource := fetchManagerResource(t, manager, sessionID, parsedMaster.Variants[0].URI)
	mediaBody, err := io.ReadAll(mediaResource.Body)
	if err != nil {
		t.Fatalf("read rewritten media playlist: %v", err)
	}
	_ = mediaResource.Body.Close()

	mediaText := string(mediaBody)
	if strings.Contains(mediaText, "media.example") {
		t.Fatalf("rewritten media playlist leaked upstream URL: %s", mediaText)
	}
	if !strings.Contains(mediaText, `URI="/api/v1/live/playback/session/`) {
		t.Fatalf("key URI was not rewritten: %s", mediaText)
	}

	var segmentResourceURL string
	for _, line := range strings.Split(mediaText, "\n") {
		if strings.HasPrefix(line, "/api/v1/live/playback/session/") {
			segmentResourceURL = line
			break
		}
	}
	if segmentResourceURL == "" {
		t.Fatalf("segment URI was not rewritten: %s", mediaText)
	}

	segmentResource := fetchManagerResource(t, manager, sessionID, segmentResourceURL)
	segmentBody, err := io.ReadAll(segmentResource.Body)
	if err != nil {
		t.Fatalf("read relayed segment: %v", err)
	}
	_ = segmentResource.Body.Close()
	if got := string(segmentBody); got != "video-segment" {
		t.Fatalf("segment body = %q, want %q", got, "video-segment")
	}
}

func TestManagerRejectsTamperedResourceSignature(t *testing.T) {
	t.Parallel()

	master, err := hls.DecodeMultivariant(strings.NewReader(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS="avc1.42e01e",VIDEO="source"
https://media.example/quality.m3u8
`))
	if err != nil {
		t.Fatalf("decode master playlist: %v", err)
	}

	manager := newManager(func(context.Context, string) (*Source, error) {
		return &Source{
			Playlist: master,
			Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatalf("tampered resource unexpectedly reached upstream: %s", req.URL)
				return nil, nil
			})},
			Headers: make(http.Header),
		}, nil
	}, time.Hour)

	sessionID, err := manager.Start(context.Background(), "example")
	if err != nil {
		t.Fatalf("start playback session: %v", err)
	}
	rewrittenMaster, err := manager.Master(sessionID)
	if err != nil {
		t.Fatalf("get rewritten master: %v", err)
	}
	parsedMaster, err := hls.DecodeMultivariant(strings.NewReader(string(rewrittenMaster)))
	if err != nil {
		t.Fatalf("decode rewritten master: %v", err)
	}

	resourceURL, err := url.Parse(parsedMaster.Variants[0].URI)
	if err != nil {
		t.Fatalf("parse resource URL: %v", err)
	}
	query := resourceURL.Query()
	query.Set("signature", "tampered")

	_, err = manager.Fetch(
		context.Background(),
		sessionID,
		query.Get("url"),
		query.Get("signature"),
		query,
		"",
	)
	if err != ErrInvalidResource {
		t.Fatalf("Fetch() error = %v, want %v", err, ErrInvalidResource)
	}
}

func fetchManagerResource(
	t *testing.T,
	manager *Manager,
	sessionID string,
	resourceURL string,
) *Resource {
	t.Helper()

	parsedURL, err := url.Parse(resourceURL)
	if err != nil {
		t.Fatalf("parse rewritten resource URL: %v", err)
	}
	query := parsedURL.Query()

	resource, err := manager.Fetch(
		context.Background(),
		sessionID,
		query.Get("url"),
		query.Get("signature"),
		query,
		"",
	)
	if err != nil {
		t.Fatalf("fetch rewritten resource: %v", err)
	}
	return resource
}
