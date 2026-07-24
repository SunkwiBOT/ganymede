"use client";

import { useEffect, useState } from "react";

function formatElapsedTime(totalSeconds: number): string {
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  return [hours, minutes, seconds]
    .map((value) => value.toString().padStart(2, "0"))
    .join(":");
}

export default function LiveElapsedTimer({
  streamedAt,
}: {
  streamedAt: string | Date;
}) {
  const [elapsedSeconds, setElapsedSeconds] = useState<number | null>(null);

  useEffect(() => {
    const startedAt = new Date(streamedAt).getTime();

    const updateElapsedTime = () => {
      if (!Number.isFinite(startedAt)) {
        setElapsedSeconds(0);
        return;
      }

      setElapsedSeconds(
        Math.max(0, Math.floor((Date.now() - startedAt) / 1000)),
      );
    };

    updateElapsedTime();
    const intervalID = window.setInterval(updateElapsedTime, 1000);

    return () => window.clearInterval(intervalID);
  }, [streamedAt]);

  return elapsedSeconds === null
    ? "--:--:--"
    : formatElapsedTime(elapsedSeconds);
}
