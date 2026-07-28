package tasks

import (
	"time"

	"github.com/riverqueue/river/rivertype"
)

// RiverJobArgs is the common JSON shape used when inspecting archive jobs
// without knowing their concrete argument type.
type RiverJobArgs struct {
	VideoId  string            `json:"video_id"`
	Input    ArchiveVideoInput `json:"input"`
	Continue bool              `json:"continue"`
}

// isLiveArchiveJobStale supports jobs created before archive middleware
// heartbeats were introduced. Falling back to River's timestamps prevents a
// crash before the first legacy heartbeat from blocking a continuation
// recording forever.
func isLiveArchiveJobStale(job *rivertype.JobRow, args RiverJobArgs, now time.Time) bool {
	if job == nil {
		return true
	}

	lastHeartbeat := args.Input.HeartBeatTime
	if lastHeartbeat.IsZero() {
		if job.AttemptedAt != nil {
			lastHeartbeat = *job.AttemptedAt
		} else {
			lastHeartbeat = job.CreatedAt
		}
	}
	if lastHeartbeat.IsZero() {
		return false
	}

	return now.Sub(lastHeartbeat) > archiveHeartbeatTimeout
}
