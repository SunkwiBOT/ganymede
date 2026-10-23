"use client"
import { ArchiveStatus, isArchiveActive } from "@/app/util/archiveStatus";
import { Platform, useFetchVideo, useGetVideoClips, VideoType } from "@/app/hooks/useVideos";
import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import classes from "./VideoPage.module.css"
import { Box, Container, useComputedColorScheme, useMantineTheme } from "@mantine/core";
import RecordingNotice from "@/app/components/videos/RecordingNotice";
import VideoPlayer from "@/app/components/videos/Player";
import VideoTitleBar from "@/app/components/videos/TitleBar";
import ChatPlayer from "@/app/components/videos/ChatPlayer";
import GanymedeLoadingText from "@/app/components/utils/GanymedeLoadingText";
import useSettingsStore from "@/app/store/useSettingsStore";
import { useFullscreenDocument, useMediaQuery } from "@mantine/hooks";
import { env } from "next-runtime-env";
import VideoLoginRequired from "@/app/components/videos/LoginRequired";
import useAuthStore from "@/app/store/useAuthStore";
import VideoPageClips from "@/app/components/videos/VideoClips";
import VideoChatHistogram from "@/app/components/videos/ChatHistogram";
import VideoNotes from "@/app/components/videos/VideoNotes";
import { useTranslations } from "next-intl";

interface Params {
  id: string;
}

const TwitchLiveChat = ({ channelName }: { channelName: string }) => {
  const [parentHost, setParentHost] = useState<string | null>(null);
  const computedColorScheme = useComputedColorScheme('dark', { getInitialValueInEffect: true });

  useEffect(() => {
    setParentHost(window.location.hostname);
  }, []);

  const chatSrc = useMemo(() => {
    if (!parentHost || !channelName) return "";

    const params = new URLSearchParams();
    params.append("parent", parentHost);
    if (computedColorScheme === "dark") {
      params.append("darkpopout", "");
    }

    return `https://www.twitch.tv/embed/${encodeURIComponent(channelName)}/chat?${params.toString()}`;
  }, [channelName, computedColorScheme, parentHost]);

  if (!chatSrc) return null;

  return (
    <iframe
      className={classes.twitchChatEmbed}
      src={chatSrc}
      title={`Twitch chat ${channelName}`}
    />
  );
};

type NotesDraft = {
  videoId: string;
  value: string;
};

const VideoPage = ({ params }: { params: Promise<Params> }) => {
  const theme = useMantineTheme()
  const { id } = React.use(params);
  const { isLoggedIn } = useAuthStore()
  const player = useRef<HTMLVideoElement>(null);
  const [notesPanelOpen, setNotesPanelOpen] = useState(false);
  const [notesDraft, setNotesDraft] = useState<NotesDraft | null>(null);
  const isMobile = useMediaQuery(`(max-width: ${theme.breakpoints.sm})`);

  const t = useTranslations("VideoPage");
  const loginRequired = env('NEXT_PUBLIC_REQUIRE_LOGIN') == "true" && !isLoggedIn;

  const videoTheaterMode = useSettingsStore((state) => state.videoTheaterMode);
  const hideChat = useSettingsStore((state) => state.hideChat);
  const showChatHistogram = useSettingsStore((state) => state.showChatHistogram);
  const chatOnLeft = useSettingsStore((state) => state.chatOnLeft);
  const { fullscreen } = useFullscreenDocument();

  const { data, isPending, isError } = useFetchVideo({ id, with_channel: true, with_chapters: true, with_muted_segments: true, enabled: !loginRequired })

  // need to fetch clips here to dynamically render the clips section
  const { data: videoClips, isPending: videoClipsPending, isError: videoClipsError } = useGetVideoClips(id, !loginRequired)

  const openNotesPanel = useCallback(() => {
    setNotesPanelOpen(true);
    requestAnimationFrame(() => {
      window.scrollTo({ top: 0, behavior: "smooth" });
    });
  }, []);

  useEffect(() => {
    document.title = `${data?.title}`;
  }, [data?.title]);

  if (loginRequired) {
    return <VideoLoginRequired />
  }

  if (isPending) {
    return <GanymedeLoadingText message={t('loading')} />
  }
  if (isError) {
    return <div>{t('error')}</div>
  }

  const showArchivedChat = Boolean(data.chat_path && data.status === ArchiveStatus.Completed);
  const showTwitchLiveChat = Boolean(
    isArchiveActive(data.status) &&
    data.type === VideoType.Live &&
    data.platform === Platform.Twitch &&
    data.edges.channel.name
  );
  const showChat = !hideChat && (showArchivedChat || showTwitchLiveChat);

  const currentNotesDraft = notesDraft?.videoId === data.id
    ? notesDraft.value
    : (data.notes ?? "");
  const handleNotesDraftChange = (value: string) => {
    setNotesDraft({ videoId: data.id, value });
  };

  return (
    <div>
      {/* Player and chat section — single tree on both layouts so VideoPlayer/ChatPlayer instances persist across the breakpoint flip */}
      <Box
        className={
          isMobile
            ? classes.containerMobile
            : `${classes.container} ${chatOnLeft ? classes.containerChatOnLeft : ""}`
        }
      >
        {/* Player */}
        <div className={
          isMobile
            ? undefined
            : (!showChat && !notesPanelOpen ? classes.leftColumnNoChat : classes.leftColumn)
        }>
          <div className={
            isMobile
              ? undefined
              : (videoTheaterMode || fullscreen ? classes.videoPlayerTheaterMode : classes.videoPlayer)
          }>
            {isArchiveActive(data.status) && !data.live_preview_available ? (
              <RecordingNotice video={data} />
            ) : (
              <VideoPlayer key={data.id} video={data} ref={player} />
            )}
          </div>
        </div>

        {/* Chat */}
        {!isMobile && notesPanelOpen ? (
          <div className={classes.rightColumn}>
            <VideoNotes
              video={data}
              playerRef={player}
              draft={currentNotesDraft}
              onDraftChange={handleNotesDraftChange}
              docked
              onClose={() => setNotesPanelOpen(false)}
            />
          </div>
        ) : showChat && (
          <div
            className={isMobile ? classes.chatColumnMobile : classes.rightColumn}
            style={isMobile ? undefined : { height: "auto", maxHeight: "auto" }}
          >
            <div
              className={
                isMobile
                  ? undefined
                  : (videoTheaterMode || fullscreen ? classes.chatColumnTheaterMode : classes.chatColumn)
              }
              style={isMobile ? { height: "100%" } : undefined}
            >
              {showArchivedChat ? (
                <ChatPlayer key={data.id} video={data} playerRef={player} />
              ) : (
                <TwitchLiveChat channelName={data.edges.channel.name} />
              )}
            </div>
          </div>
        )}
      </Box>

      {/* Title bar */}
      {!videoTheaterMode && (
        <VideoTitleBar
          video={data}
          onEditNotes={!isMobile ? openNotesPanel : undefined}
        />
      )}

      {/* Desktop-only sections render after the player/chat block so toggling them doesn't shift player position */}
      {!isMobile && data.status === ArchiveStatus.Completed && (
        <Container size="7xl" fluid={true} >
          {videoClipsError && (
            <div>Error loading clips</div>
          )}
          {((!videoClipsPending) && (videoClips && videoClips.length > 0)) && (
            <VideoPageClips clips={videoClips} />
          )}
        </Container>
      )}

      {(!isMobile && data.chat_path && (data.type != VideoType.Clip) && showChatHistogram && data.status === ArchiveStatus.Completed) && (
        <Container size="7xl" fluid={true} >
          <VideoChatHistogram videoId={data.id} playerRef={player} />
        </Container>
      )}

      {/* Notes live under the player (below chat histogram) and render on all layouts */}
      <Container size="7xl" fluid={true} >
        <VideoNotes
          video={data}
          playerRef={player}
          draft={currentNotesDraft}
          onDraftChange={handleNotesDraftChange}
          onEdit={!isMobile ? openNotesPanel : undefined}
        />
      </Container>

      {!isMobile && (
        <style jsx>{`
          :global(html)::-webkit-scrollbar {
            display: none;
          }
          :global(html) {
            -ms-overflow-style: none; /* IE and Edge */
            scrollbar-width: none; /* Firefox */
          }
        `}</style>
      )}
    </div>
  );
}

export default VideoPage;
