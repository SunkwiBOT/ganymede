package livestream

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zibbp/ganymede/internal/hls"
)

const (
	defaultSessionTTL = 30 * time.Minute
	resourceRoute     = "/api/v1/live/playback/session/%s/resource"
	maxMediaPlaylist  = 2 * hls.MaxPlaylistSize
)

var (
	ErrSessionNotFound = errors.New("live playback session not found")
	ErrInvalidResource = errors.New("invalid live playback resource")

	hlsURIAttributePattern = regexp.MustCompile(`URI="([^"]+)"`)
)

type sourceResolver func(context.Context, string) (*Source, error)

type playbackSession struct {
	source   *Source
	master   []byte
	secret   []byte
	lastUsed time.Time
	timer    *time.Timer
}

// Manager owns short-lived playback sessions. Upstream URLs are signed and
// relayed through Ganymede so proxy credentials and Twitch playback tokens
// never need to be exposed directly to the browser.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*playbackSession
	resolve  sourceResolver
	ttl      time.Duration
	now      func() time.Time
}

type Resource struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

func NewManager() *Manager {
	return newManager(Resolve, defaultSessionTTL)
}

func newManager(resolve sourceResolver, ttl time.Duration) *Manager {
	return &Manager{
		sessions: make(map[string]*playbackSession),
		resolve:  resolve,
		ttl:      ttl,
		now:      time.Now,
	}
}

// Start resolves a channel, creates an ephemeral playback session and returns
// the opaque session ID used to retrieve its rewritten master playlist.
func (m *Manager) Start(ctx context.Context, channelName string) (string, error) {
	source, err := m.resolve(ctx, channelName)
	if err != nil {
		return "", err
	}
	if source == nil || source.Playlist == nil || source.Client == nil {
		return "", fmt.Errorf("resolved live stream is incomplete")
	}

	sessionID := uuid.NewString()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("failed to create live playback session secret: %w", err)
	}

	session := &playbackSession{
		source:   source,
		secret:   secret,
		lastUsed: m.now(),
	}

	if err := rewriteMasterPlaylist(sessionID, session); err != nil {
		source.Client.CloseIdleConnections()
		return "", err
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	session.timer = time.AfterFunc(m.ttl, func() {
		m.expire(sessionID)
	})
	m.mu.Unlock()

	return sessionID, nil
}

func (m *Manager) Master(sessionID string) ([]byte, error) {
	session, err := m.touch(sessionID)
	if err != nil {
		return nil, err
	}

	return append([]byte(nil), session.master...), nil
}

// Fetch validates a signed upstream URL and retrieves it with the client,
// proxy and custom header selected for this session.
func (m *Manager) Fetch(
	ctx context.Context,
	sessionID string,
	encodedURL string,
	signature string,
	extraQuery url.Values,
	rangeHeader string,
) (*Resource, error) {
	session, err := m.touch(sessionID)
	if err != nil {
		return nil, err
	}

	upstreamURL, err := decodeAndVerifyResource(session, encodedURL, signature)
	if err != nil {
		return nil, err
	}

	for _, key := range []string{"_HLS_msn", "_HLS_part", "_HLS_skip"} {
		if value := extraQuery.Get(key); value != "" {
			query := upstreamURL.Query()
			query.Set(key, value)
			upstreamURL.RawQuery = query.Encode()
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create live playback resource request: %w", err)
	}
	applyHeaders(req, session.source.Headers)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}

	resp, err := session.source.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch live playback resource: %w", err)
	}

	if !isHLSPlaylistResponse(resp) || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Resource{
			StatusCode: resp.StatusCode,
			Header:     responseHeaders(resp.Header, false),
			Body:       resp.Body,
		}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaPlaylist+1))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to read live media playlist: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close live media playlist response: %w", closeErr)
	}
	if len(body) > maxMediaPlaylist {
		return nil, fmt.Errorf("live media playlist exceeds maximum size of %d bytes", maxMediaPlaylist)
	}

	rewritten, err := rewriteMediaPlaylist(sessionID, session, string(body), resp.Request.URL)
	if err != nil {
		return nil, err
	}

	return &Resource{
		StatusCode: resp.StatusCode,
		Header:     responseHeaders(resp.Header, true),
		Body:       io.NopCloser(strings.NewReader(rewritten)),
	}, nil
}

func (m *Manager) touch(sessionID string) (*playbackSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}

	session.lastUsed = m.now()
	if session.timer != nil {
		session.timer.Reset(m.ttl)
	}

	return session, nil
}

func (m *Manager) expire(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[sessionID]
	if !ok {
		return
	}

	idleFor := m.now().Sub(session.lastUsed)
	if idleFor < m.ttl {
		session.timer.Reset(m.ttl - idleFor)
		return
	}

	delete(m.sessions, sessionID)
	session.source.Client.CloseIdleConnections()
}

func rewriteMasterPlaylist(sessionID string, session *playbackSession) error {
	for _, variant := range session.source.Playlist.Variants {
		upstreamURL, err := resolveUpstreamURL(session.source.BaseURL, variant.URI)
		if err != nil {
			return fmt.Errorf("failed to resolve live stream variant: %w", err)
		}
		variant.URI = signedResourceURL(sessionID, session.secret, upstreamURL.String())
	}

	for _, rendition := range session.source.Playlist.Renditions {
		if rendition.URI == nil || *rendition.URI == "" {
			continue
		}

		upstreamURL, err := resolveUpstreamURL(session.source.BaseURL, *rendition.URI)
		if err != nil {
			return fmt.Errorf("failed to resolve live stream rendition: %w", err)
		}
		rewritten := signedResourceURL(sessionID, session.secret, upstreamURL.String())
		rendition.URI = &rewritten
	}

	master, err := session.source.Playlist.Marshal()
	if err != nil {
		return fmt.Errorf("failed to encode live stream master playlist: %w", err)
	}
	session.master = master
	return nil
}

func rewriteMediaPlaylist(
	sessionID string,
	session *playbackSession,
	playlistText string,
	baseURL *url.URL,
) (string, error) {
	var result strings.Builder

	for _, lineWithNewline := range strings.SplitAfter(playlistText, "\n") {
		line := strings.TrimSuffix(lineWithNewline, "\n")
		newline := lineWithNewline[len(line):]
		lineWithoutCR := strings.TrimSuffix(line, "\r")
		cr := line[len(lineWithoutCR):]
		trimmed := strings.TrimSpace(lineWithoutCR)

		switch {
		case trimmed == "":
			// Keep blank lines untouched.
		case strings.HasPrefix(trimmed, "#"):
			var rewriteErr error
			lineWithoutCR = hlsURIAttributePattern.ReplaceAllStringFunc(lineWithoutCR, func(match string) string {
				if rewriteErr != nil {
					return match
				}

				groups := hlsURIAttributePattern.FindStringSubmatch(match)
				upstreamURL, err := resolveUpstreamURL(baseURL, groups[1])
				if err != nil {
					rewriteErr = err
					return match
				}

				return `URI="` + signedResourceURL(sessionID, session.secret, upstreamURL.String()) + `"`
			})
			if rewriteErr != nil {
				return "", fmt.Errorf("failed to rewrite live playlist URI attribute: %w", rewriteErr)
			}
		default:
			upstreamURL, err := resolveUpstreamURL(baseURL, trimmed)
			if err != nil {
				return "", fmt.Errorf("failed to rewrite live playlist URI: %w", err)
			}

			leadingWhitespace := lineWithoutCR[:len(lineWithoutCR)-len(strings.TrimLeft(lineWithoutCR, " \t"))]
			trailingWhitespace := lineWithoutCR[len(strings.TrimRight(lineWithoutCR, " \t")):]
			lineWithoutCR = leadingWhitespace +
				signedResourceURL(sessionID, session.secret, upstreamURL.String()) +
				trailingWhitespace
		}

		result.WriteString(lineWithoutCR)
		result.WriteString(cr)
		result.WriteString(newline)
	}

	return result.String(), nil
}

func resolveUpstreamURL(baseURL *url.URL, value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if !parsed.IsAbs() {
		if baseURL == nil {
			return nil, fmt.Errorf("relative URI %q has no base URL", value)
		}
		parsed = baseURL.ResolveReference(parsed)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported upstream URI scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("upstream URI has no host")
	}
	return parsed, nil
}

func signedResourceURL(sessionID string, secret []byte, upstreamURL string) string {
	encodedURL := base64.RawURLEncoding.EncodeToString([]byte(upstreamURL))
	signature := signResource(secret, encodedURL)
	values := url.Values{
		"url":       []string{encodedURL},
		"signature": []string{signature},
	}
	return fmt.Sprintf(resourceRoute, sessionID) + "?" + values.Encode()
}

func decodeAndVerifyResource(
	session *playbackSession,
	encodedURL string,
	signature string,
) (*url.URL, error) {
	expectedSignature := signResource(session.secret, encodedURL)
	providedSignature, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return nil, ErrInvalidResource
	}
	expectedSignatureBytes, err := base64.RawURLEncoding.DecodeString(expectedSignature)
	if err != nil || !hmac.Equal(providedSignature, expectedSignatureBytes) {
		return nil, ErrInvalidResource
	}

	decodedURL, err := base64.RawURLEncoding.DecodeString(encodedURL)
	if err != nil {
		return nil, ErrInvalidResource
	}

	upstreamURL, err := resolveUpstreamURL(nil, string(decodedURL))
	if err != nil {
		return nil, ErrInvalidResource
	}
	return upstreamURL, nil
}

func signResource(secret []byte, encodedURL string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(encodedURL))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func isHLSPlaylistResponse(resp *http.Response) bool {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch strings.ToLower(mediaType) {
	case "application/vnd.apple.mpegurl",
		"application/x-mpegurl",
		"audio/mpegurl",
		"audio/x-mpegurl":
		return true
	}

	return strings.EqualFold(path.Ext(resp.Request.URL.Path), ".m3u8")
}

func responseHeaders(upstream http.Header, rewrittenPlaylist bool) http.Header {
	headers := make(http.Header)
	for _, name := range []string{
		"Accept-Ranges",
		"Cache-Control",
		"Content-Length",
		"Content-Range",
		"Content-Type",
		"ETag",
		"Last-Modified",
	} {
		for _, value := range upstream.Values(name) {
			headers.Add(name, value)
		}
	}

	if rewrittenPlaylist {
		headers.Del("Content-Length")
		headers.Del("Content-Range")
		headers.Del("ETag")
		headers.Set("Content-Type", "application/vnd.apple.mpegurl")
		headers.Set("Cache-Control", "no-store")
	}

	return headers
}
