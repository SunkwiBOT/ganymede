import '@vidstack/react/player/styles/default/theme.css';
import '@vidstack/react/player/styles/default/layouts/video.css';
import { MediaPlayer, MediaPlayerInstance, MediaProvider, MediaSrc, Poster, Track, VideoMimeType, useMediaState } from '@vidstack/react';
import { defaultLayoutIcons, DefaultVideoLayout } from '@vidstack/react/player/layouts/default';
import { Video, VideoType } from '@/app/hooks/useVideos';
import classes from "./Player.module.css"
import { RefObject, useEffect, useMemo, useRef, useState } from 'react';
import { env } from 'next-runtime-env';
import dayjs from 'dayjs';
import { escapeURL } from '@/app/util/util';
import { PlaybackStatus, useFetchPlaybackForVideo, useSetPlaybackProgressForVideo, useUpdatePlaybackProgressForVideo } from '@/app/hooks/usePlayback';
import { useAxiosPrivate } from '@/app/hooks/useAxios';
import useAuthStore from '@/app/store/useAuthStore';
import { useSearchParams } from 'next/navigation';
import VideoEventBus from '@/app/util/VideoEventBus';
import VideoPlayerTheaterModeIcon from './PlayerTheaterModeIcon';
import useSettingsStore from '@/app/store/useSettingsStore';
import VideoPlayerHideChatIcon from './PlayerHideChatIcon';
import VideoPlayerAbsoluteTimeIcon from './PlayerAbsoluteTimeIcon';

interface Params {
  video: Video;
  ref: RefObject<MediaPlayerInstance | null>;
}

const LIVE_EDGE_FALLBACK_DELAY_SECONDS = 2;
// The temporary live playlist uses two-second HLS segments. Keep the DVR
// control enabled as soon as at least one segment is available.
const LIVE_DVR_MIN_WINDOW_SECONDS = 2;

const seekToProcessingLiveEdge = (player: MediaPlayerInstance): boolean => {
  try {
    player.seekToLiveEdge();
    return true;
  } catch {
    // The player can throw if the provider is not ready yet; fall back below.
  }

  const seekableEnd = player.state.seekableEnd;
  if (!Number.isFinite(seekableEnd) || seekableEnd <= 0) {
    return false;
  }

  player.currentTime = Math.max(0, seekableEnd - LIVE_EDGE_FALLBACK_DELAY_SECONDS);
  return true;
};

const AbsoluteTimeDisplay = ({ streamedAt }: { streamedAt: string | Date }) => {
  const currentTime = useMediaState('currentTime');
  const flooredCurrentTime = Math.floor(currentTime);
  const absoluteTime = useMemo(
    () => dayjs(streamedAt).add(flooredCurrentTime, 'second'),
    [streamedAt, flooredCurrentTime],
  );

  return (
    <div className={classes.absoluteTimeOverlay}>
      <span className={classes.absoluteTimeText}>{absoluteTime.format('YYYY-MM-DD HH:mm:ss')}</span>
    </div>
  );
};

const VideoPlayer = ({ video, ref }: Params) => {
  const searchParams = useSearchParams()

  const isLoggedIn = useAuthStore(state => state.isLoggedIn);

  const player = ref;
  const [videoSource, setVideoSource] = useState<MediaSrc>();
  const [videoPoster, setVideoPoster] = useState<string>("");

  const hasInitializedPlaybackTime = useRef(false);
  const returnToLiveOnPlay = useRef(false);

  const [playerVolume, setPlayerVolume] = useState(1);

  const updatePlaybackProgressMutation = useUpdatePlaybackProgressForVideo()
  const setPlaybackProgressMutation = useSetPlaybackProgressForVideo()

  const videoTheaterMode = useSettingsStore((state) => state.videoTheaterMode);
  const showAbsoluteTime = useSettingsStore((state) => state.showAbsoluteTime);
  const autoplayVideo = useSettingsStore((state) => state.autoplayVideo);
  const isProcessingLive = video.processing && video.type === VideoType.Live;
  const urlStartTime = useMemo(() => {
    const time = searchParams.get("t");
    if (time === null) return null;

    const parsedTime = parseInt(time, 10);
    return Number.isFinite(parsedTime) ? parsedTime : null;
  }, [searchParams]);

  const axiosPrivate = useAxiosPrivate();
  // get playback data
  const { data: playbackData } = useFetchPlaybackForVideo(axiosPrivate, video.id, {
    refetchOnMount: "always",
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    retry: false,
    enabled: (isLoggedIn)
  })

  useEffect(() => {
    if (!player) return

    const videoPath = video.video_path ?? "";
    const videoExtension = videoPath.substring(videoPath.length - 4)
    let videoType: VideoMimeType = "video/mp4"
    if (videoExtension == "m3u8") {
      videoType = "video/object";
    }

    // Only processing live streams are watchable while archiving through the temporary HLS output.
    if (isProcessingLive && video.tmp_video_hls_path) {
      setVideoSource({
        src: `${(env('NEXT_PUBLIC_CDN_URL') ?? '')}${escapeURL(video.tmp_video_hls_path)}/${video.ext_id}-video.m3u8`,
        type: "application/x-mpegurl"
      })
    } else if (!video.processing && videoPath) {
      setVideoSource({
        src: `${(env('NEXT_PUBLIC_CDN_URL') ?? '')}${escapeURL(videoPath)}`,
        type: videoType
      })
    } else {
      setVideoSource(undefined);
    }

    if (video.thumbnail_path) {
      setVideoPoster(`${(env('NEXT_PUBLIC_CDN_URL') ?? '')}${escapeURL(video.thumbnail_path)}`)
    }

    // todo: captions?

    const localVolume = localStorage.getItem("ganymede-volume")
    if (localVolume) {
      setPlayerVolume(parseFloat(localVolume))
    }

    const unsubscribe = player.current?.subscribe(({ volume }) => {
      if (volume != 1) {
        localStorage.setItem("ganymede-volume", volume.toString());
      }
    });

    return unsubscribe;
  }, [isProcessingLive, player, video])

  useEffect(() => {
    const currentPlayer = player.current;
    if (!currentPlayer || hasInitializedPlaybackTime.current) return;

    if (!hasInitializedPlaybackTime.current) {
      if (isProcessingLive) {
        if (urlStartTime !== null) {
          currentPlayer.currentTime = urlStartTime;
          hasInitializedPlaybackTime.current = true;
          return;
        }

        if (currentPlayer.state.canPlay) {
          hasInitializedPlaybackTime.current = seekToProcessingLiveEdge(currentPlayer);
        }

        if (hasInitializedPlaybackTime.current) return;

        const unsubscribe = currentPlayer.subscribe(({ canPlay, seekableEnd }) => {
          if (hasInitializedPlaybackTime.current || !canPlay) return;
          if (!Number.isFinite(seekableEnd) || seekableEnd <= 0) return;

          hasInitializedPlaybackTime.current = seekToProcessingLiveEdge(currentPlayer);
        });

        return unsubscribe;
      }

      if (playbackData && playbackData.time != null) {
        // Resume from server-side playback progress.
        currentPlayer.currentTime = playbackData.time
        hasInitializedPlaybackTime.current = true
      } else if (urlStartTime !== null) {
        currentPlayer.currentTime = urlStartTime;
        hasInitializedPlaybackTime.current = true
      }
    }
  }, [isProcessingLive, playbackData, player, urlStartTime])


  // Playback progress reporting
  useEffect(() => {
    if (!isLoggedIn) return;
    const playbackInerval = setInterval(async () => {
      if (player.current == null) return;
      if (player.current.paused) return;

      const playerTimeInt = Math.floor(player.current.currentTime)
      if (playerTimeInt == 0) return;


      updatePlaybackProgressMutation.mutate({
        axiosPrivate: axiosPrivate,
        videoId: video.id,
        time: playerTimeInt
      })

      // mark video as finished if over duration threshold
      if (!video.processing && (playerTimeInt / video.duration >= 0.98)) {
        setPlaybackProgressMutation.mutate({
          axiosPrivate: axiosPrivate,
          videoId: video.id,
          status: PlaybackStatus.Finished
        })

        // remove interval
        clearInterval(playbackInerval)
      }
    }, 10000);
    return () => clearInterval(playbackInerval);

    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Fast tick for chat player - set player information in bus
  useEffect(() => {
    const ticketInterval = setInterval(() => {
      if (player.current == null) return;

      let time = player.current.state.currentTime
      // Clip chats are offset with the position of the clip in the VOD
      // Append the offset to the current player time to account for this
      if (video.type == VideoType.Clip && video.clip_vod_offset) {
        time = time + video.clip_vod_offset
      };

      VideoEventBus.setData({
        isPaused: player.current.state.paused,
        isPlaying: player.current.state.playing,
        time: time
      })
    }, 100);
    return () => {
      clearInterval(ticketInterval);
    };
  }, [player, video.clip_vod_offset, video.type]);

  // thumbnails URL only when not processing
  const thumbnails = !video.processing
    ? `${(env('NEXT_PUBLIC_API_URL') ?? '')}/api/v1/vod/${video.id}/thumbnails/vtt`
    : undefined
  return (
    <MediaPlayer
      ref={player}
      className={
        videoTheaterMode
          ? classes.mediaPlayerTheaterMode
          : classes.mediaPlayer
      }
      src={videoSource}
      aspect-ratio={16 / 9}
      crossOrigin={true}
      playsInline={true}
      load="eager"
      posterLoad="eager"
      volume={playerVolume}
      autoPlay={autoplayVideo}
      onPause={() => {
        if (isProcessingLive) {
          returnToLiveOnPlay.current = true;
        }
      }}
      onPlay={() => {
        if (isProcessingLive && returnToLiveOnPlay.current && player.current) {
          seekToProcessingLiveEdge(player.current);
          returnToLiveOnPlay.current = false;
        }
      }}
      // Vidstack otherwise infers a non-seekable live stream from the
      // short-target-duration EVENT playlist used while archiving.
      streamType={isProcessingLive ? "live:dvr" : "unknown"}
      minLiveDVRWindow={LIVE_DVR_MIN_WINDOW_SECONDS}
    >
      {showAbsoluteTime && <AbsoluteTimeDisplay streamedAt={video.streamed_at} />}
      <MediaProvider>
        <Poster className={`${classes.mediaPlayerPoster} vds-poster`} src={videoPoster} alt={video.title} />
        {!video.processing && (
          <Track
            src={`${(env('NEXT_PUBLIC_API_URL') ?? '')}/api/v1/chapter/video/${video.id}/webvtt`}
            kind="chapters"
            default={true}
          />
        )}
      </MediaProvider>
      <DefaultVideoLayout icons={defaultLayoutIcons} noScrubGesture={false}
        slots={{
          beforeFullscreenButton: <VideoPlayerTheaterModeIcon />,
          afterFullscreenButton: (
            <>
              <VideoPlayerAbsoluteTimeIcon />
              <VideoPlayerHideChatIcon />
            </>
          )
        }}
        thumbnails={thumbnails}
      />
    </MediaPlayer>
  );
}

export default VideoPlayer;
