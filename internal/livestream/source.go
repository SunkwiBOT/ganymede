package livestream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/zibbp/ganymede/internal/config"
	"github.com/zibbp/ganymede/internal/hls"
	"github.com/zibbp/ganymede/internal/platform"
	"github.com/zibbp/ganymede/internal/utils"
)

const (
	proxyProbeTimeout   = 5 * time.Second
	resourceHTTPTimeout = 45 * time.Second
)

// Source is a resolved Twitch live stream and the HTTP configuration needed
// to retrieve its media playlists and segments.
type Source struct {
	Playlist *hls.Multivariant
	BaseURL  *url.URL
	Client   *http.Client
	Headers  http.Header
	Kind     string
}

// Resolve uses the same proxy order, whitelist and Twitch playback token as
// live recording. If every configured proxy fails, it falls back to Twitch.
func Resolve(ctx context.Context, channelName string) (*Source, error) {
	cfg := config.Get()
	twitchURL := utils.CreateTwitchURL("", utils.Live, channelName)

	if cfg != nil && cfg.Livestream.ProxyEnabled {
		if utils.Contains(cfg.Livestream.ProxyWhitelist, channelName) {
			log.Debug().Str("channel_name", channelName).Msg("channel is whitelisted, not using proxy")
		} else {
			log.Debug().
				Str("proxy_list", fmt.Sprintf("%v", cfg.Livestream.Proxies)).
				Msg("proxy list")

			for _, proxy := range cfg.Livestream.Proxies {
				proxyURL := twitchURL
				if proxy.ProxyType == utils.ProxyTypeTwitchHLS {
					proxyURL = fmt.Sprintf(
						"%s/playlist/%s.m3u8%s",
						strings.TrimRight(proxy.URL, "/"),
						channelName,
						cfg.Livestream.ProxyParameters,
					)
				}

				source, err := ResolveProxy(ctx, proxy, proxyURL)
				if err != nil {
					log.Warn().
						Err(err).
						Str("channel_name", channelName).
						Str("proxy_url", proxy.URL).
						Msg("live stream proxy is unavailable")
					continue
				}

				log.Debug().
					Str("channel_name", channelName).
					Str("proxy_url", proxy.URL).
					Msg("live stream proxy selected")
				return source, nil
			}
		}
	}

	tc := &platform.TwitchConnection{}
	masterPlaylist, err := tc.GetStream(ctx, channelName)
	if err != nil {
		return nil, fmt.Errorf("failed to get direct Twitch stream: %w", err)
	}

	headers := make(http.Header)
	headers.Set("User-Agent", utils.ChromeUserAgent)

	return &Source{
		Playlist: masterPlaylist,
		Client:   newHTTPClient(cloneDefaultTransport(), headers),
		Headers:  headers,
		Kind:     "direct",
	}, nil
}

// ResolveProxy probes one configured proxy and returns the HTTP client and
// headers that must also be used for its playlists and media segments.
func ResolveProxy(ctx context.Context, proxy config.ProxyListItem, testURL string) (*Source, error) {
	headers, err := parseProxyHeader(proxy.Header)
	if err != nil {
		return nil, err
	}
	headers.Set("User-Agent", utils.ChromeUserAgent)

	var transport http.RoundTripper = cloneDefaultTransport()
	switch proxy.ProxyType {
	case utils.ProxyTypeTwitchHLS:
		// The playlist URL itself targets the Twitch-HLS proxy.
	case utils.ProxyTypeHTTP:
		parsedProxyURL, err := url.Parse(proxy.URL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse HTTP proxy URL: %w", err)
		}
		if parsedProxyURL.Scheme != "http" && parsedProxyURL.Scheme != "https" {
			return nil, fmt.Errorf("unsupported HTTP proxy scheme %q", parsedProxyURL.Scheme)
		}

		transport = &http.Transport{
			Proxy: http.ProxyURL(parsedProxyURL),
			DialContext: (&net.Dialer{
				Timeout: proxyProbeTimeout,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		}
	default:
		return nil, fmt.Errorf("unknown proxy type %q", proxy.ProxyType)
	}

	client := newHTTPClient(transport, headers)
	probeCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, testURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create proxy playlist request: %w", err)
	}
	applyHeaders(req, headers)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch proxy playlist: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy playlist returned status code %d", resp.StatusCode)
	}

	masterPlaylist, err := hls.DecodeMultivariant(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode proxy playlist: %w", err)
	}

	kind := "twitch_hls_proxy"
	if proxy.ProxyType == utils.ProxyTypeHTTP {
		kind = "http_proxy"
	}

	return &Source{
		Playlist: masterPlaylist,
		BaseURL:  resp.Request.URL,
		Client:   client,
		Headers:  headers,
		Kind:     kind,
	}, nil
}

func newHTTPClient(transport http.RoundTripper, headers http.Header) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}

	return &http.Client{
		Transport: transport,
		Timeout:   resourceHTTPTimeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			applyHeaders(req, headers)
			return nil
		},
	}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{}
}

func parseProxyHeader(header string) (http.Header, error) {
	headers := make(http.Header)
	header = strings.TrimSpace(header)
	if header == "" {
		return headers, nil
	}

	name, value, found := strings.Cut(header, ":")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return nil, fmt.Errorf("proxy header must use the name:value format")
	}

	headers.Set(name, strings.TrimSpace(value))
	return headers, nil
}

func applyHeaders(req *http.Request, headers http.Header) {
	for name, values := range headers {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
}
