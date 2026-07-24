import { useQuery } from "@tanstack/react-query";
import useAxios, { ApiResponse } from "./useAxios";

export interface LivePlaybackSession {
  login: string;
  display_name: string;
  title: string;
  started_at?: string;
  profile_image_url?: string;
  playback_url: string;
}

const startLivePlayback = async (
  login: string,
): Promise<LivePlaybackSession> => {
  const response = await useAxios.get<ApiResponse<LivePlaybackSession>>(
    `/api/v1/live/playback/${encodeURIComponent(login)}`,
  );

  return response.data.data;
};

export const useLivePlayback = (login: string) => {
  return useQuery({
    queryKey: ["live-playback", login],
    queryFn: () => startLivePlayback(login),
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
};
