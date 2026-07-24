package live_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	entQueue "github.com/zibbp/ganymede/ent/queue"
	"github.com/zibbp/ganymede/ent/vod"
	"github.com/zibbp/ganymede/internal/archive"
	"github.com/zibbp/ganymede/internal/platform"
	"github.com/zibbp/ganymede/internal/utils"
	"github.com/zibbp/ganymede/tests"
)

type fakeLivePlatform struct {
	stream platform.LiveStreamInfo
}

func (f fakeLivePlatform) Authenticate(ctx context.Context) (*platform.ConnectionInfo, error) {
	return &platform.ConnectionInfo{}, nil
}

func (f fakeLivePlatform) GetVideo(ctx context.Context, id string, withChapters bool, withMutedSegments bool) (*platform.VideoInfo, error) {
	return nil, fmt.Errorf("unexpected GetVideo call")
}

func (f fakeLivePlatform) GetLiveStream(ctx context.Context, channelName string) (*platform.LiveStreamInfo, error) {
	return &f.stream, nil
}

func (f fakeLivePlatform) GetLiveStreams(ctx context.Context, channelIDs []string) ([]platform.LiveStreamInfo, error) {
	return []platform.LiveStreamInfo{f.stream}, nil
}

func (f fakeLivePlatform) GetChannel(ctx context.Context, channelName *string, channelID *string) (*platform.ChannelInfo, error) {
	return nil, fmt.Errorf("unexpected GetChannel call")
}

func (f fakeLivePlatform) GetVideos(ctx context.Context, channelId string, videoType platform.VideoType, withChapters bool, withMutedSegments bool) ([]platform.VideoInfo, error) {
	return nil, fmt.Errorf("unexpected GetVideos call")
}

func (f fakeLivePlatform) GetCategories(ctx context.Context) ([]platform.Category, error) {
	return nil, fmt.Errorf("unexpected GetCategories call")
}

func (f fakeLivePlatform) GetGlobalBadges(ctx context.Context) ([]platform.Badge, error) {
	return nil, fmt.Errorf("unexpected GetGlobalBadges call")
}

func (f fakeLivePlatform) GetChannelBadges(ctx context.Context, channelId string) ([]platform.Badge, error) {
	return nil, fmt.Errorf("unexpected GetChannelBadges call")
}

func (f fakeLivePlatform) GetGlobalEmotes(ctx context.Context) ([]platform.Emote, error) {
	return nil, fmt.Errorf("unexpected GetGlobalEmotes call")
}

func (f fakeLivePlatform) GetChannelEmotes(ctx context.Context, channelId string) ([]platform.Emote, error) {
	return nil, fmt.Errorf("unexpected GetChannelEmotes call")
}

func (f fakeLivePlatform) GetChannelClips(ctx context.Context, channelId string, filter platform.ClipsFilter) ([]platform.ClipInfo, error) {
	return nil, fmt.Errorf("unexpected GetChannelClips call")
}

func (f fakeLivePlatform) GetClip(ctx context.Context, id string) (*platform.ClipInfo, error) {
	return nil, fmt.Errorf("unexpected GetClip call")
}

func (f fakeLivePlatform) CheckIfStreamIsLive(ctx context.Context, channelName string) (bool, error) {
	return true, nil
}

func (f fakeLivePlatform) GetStreams(ctx context.Context, limit int) ([]platform.LiveStreamInfo, error) {
	return []platform.LiveStreamInfo{f.stream}, nil
}

func TestCheckRestartsMissingArchiveWhenChannelIsStillMarkedLive(t *testing.T) {
	app, err := tests.Setup(t)
	require.NoError(t, err)

	ctx := t.Context()
	stream := platform.LiveStreamInfo{
		ID:           "stream-missing-archive",
		UserID:       "channel-missing-archive",
		UserLogin:    "missingarchive",
		UserName:     "MissingArchive",
		GameID:       "game-missing-archive",
		GameName:     "Just Chatting",
		Type:         string(utils.Live),
		Title:        "Recording interrupted",
		ViewerCount:  42,
		StartedAt:    time.Now().Add(-5 * time.Minute),
		Language:     "en",
		ThumbnailURL: "https://example.invalid/thumb.jpg",
	}
	fakePlatform := fakeLivePlatform{stream: stream}
	app.PlatformTwitch = fakePlatform
	app.LiveService.PlatformTwitch = fakePlatform
	app.ArchiveService.PlatformTwitch = fakePlatform

	channel, err := app.Database.Client.Channel.Create().
		SetExtID(stream.UserID).
		SetName(stream.UserLogin).
		SetDisplayName(stream.UserName).
		SetImagePath("/tmp/missingarchive-profile.png").
		Save(ctx)
	require.NoError(t, err)

	watchedChannel, err := app.Database.Client.Live.Create().
		SetChannelID(channel.ID).
		SetWatchLive(true).
		SetArchiveChat(false).
		SetRenderChat(false).
		SetResolution("best").
		SetVodResolution("best").
		SetIsLive(true).
		Save(ctx)
	require.NoError(t, err)

	err = app.LiveService.Check(ctx)
	require.NoError(t, err)

	watchedChannel, err = app.Database.Client.Live.Get(ctx, watchedChannel.ID)
	require.NoError(t, err)
	assert.True(t, watchedChannel.IsLive)

	vodCount, err := app.Database.Client.Vod.Query().
		Where(vod.ExtStreamID(stream.ID)).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, vodCount)

	queueCount, err := app.Database.Client.Queue.Query().
		Where(entQueue.HasVodWith(vod.ExtStreamID(stream.ID))).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, queueCount)
}

func TestCheckReusesActiveLiveArchiveWithoutDuplicateStart(t *testing.T) {
	app, err := tests.Setup(t)
	require.NoError(t, err)

	ctx := t.Context()
	stream := platform.LiveStreamInfo{
		ID:           "stream-existing",
		UserID:       "channel-existing",
		UserLogin:    "existingchannel",
		UserName:     "ExistingChannel",
		GameID:       "game-existing",
		GameName:     "Just Chatting",
		Type:         string(utils.Live),
		Title:        "Already recording",
		ViewerCount:  42,
		StartedAt:    time.Now().Add(-5 * time.Minute),
		Language:     "en",
		ThumbnailURL: "https://example.invalid/thumb.jpg",
	}
	fakePlatform := fakeLivePlatform{stream: stream}
	app.PlatformTwitch = fakePlatform
	app.LiveService.PlatformTwitch = fakePlatform
	app.ArchiveService.PlatformTwitch = fakePlatform

	channel, err := app.Database.Client.Channel.Create().
		SetExtID(stream.UserID).
		SetName(stream.UserLogin).
		SetDisplayName(stream.UserName).
		SetImagePath("/tmp/existingchannel-profile.png").
		Save(ctx)
	require.NoError(t, err)

	watchedChannel, err := app.Database.Client.Live.Create().
		SetChannelID(channel.ID).
		SetWatchLive(true).
		SetArchiveChat(false).
		SetRenderChat(false).
		SetResolution("best").
		SetVodResolution("best").
		SetIsLive(false).
		Save(ctx)
	require.NoError(t, err)

	existingVod, err := app.Database.Client.Vod.Create().
		SetChannelID(channel.ID).
		SetExtID(stream.ID).
		SetExtStreamID(stream.ID).
		SetPlatform(utils.PlatformTwitch).
		SetType(utils.Live).
		SetTitle(stream.Title).
		SetDuration(1).
		SetResolution("best").
		SetProcessing(true).
		SetWebThumbnailPath("/tmp/existingchannel-web-thumbnail.jpg").
		SetVideoPath("/tmp/existingchannel-video.mp4").
		SetStreamedAt(stream.StartedAt).
		Save(ctx)
	require.NoError(t, err)

	existingQueue, err := app.Database.Client.Queue.Create().
		SetVodID(existingVod.ID).
		SetLiveArchive(true).
		SetProcessing(true).
		SetTaskVideoDownload(utils.Running).
		SetArchiveChat(false).
		SetRenderChat(false).
		Save(ctx)
	require.NoError(t, err)

	reusedArchive, err := app.ArchiveService.ArchiveLivestream(ctx, archive.ArchiveVideoInput{
		ChannelId:   channel.ID,
		Quality:     utils.R720,
		ArchiveChat: false,
		RenderChat:  false,
	})
	require.NoError(t, err)
	require.False(t, reusedArchive.Created)
	require.Equal(t, existingVod.ID, reusedArchive.Video.ID)
	require.Equal(t, existingQueue.ID, reusedArchive.Queue.ID)

	err = app.LiveService.Check(ctx)
	require.NoError(t, err)

	watchedChannel, err = app.Database.Client.Live.Get(ctx, watchedChannel.ID)
	require.NoError(t, err)
	assert.True(t, watchedChannel.IsLive)

	vodCount, err := app.Database.Client.Vod.Query().
		Where(vod.ExtStreamID(stream.ID)).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, vodCount)

	queueCount, err := app.Database.Client.Queue.Query().
		Where(entQueue.HasVodWith(vod.ExtStreamID(stream.ID))).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, queueCount)

	chapterCount, err := existingVod.QueryChapters().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, chapterCount)
}
