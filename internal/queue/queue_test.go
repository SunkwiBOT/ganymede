package queue

import (
	"testing"

	"github.com/zibbp/ganymede/ent"
	"github.com/zibbp/ganymede/internal/utils"
)

func TestIsActiveLiveCaptureQueue(t *testing.T) {
	tests := []struct {
		name   string
		q      *ent.Queue
		status utils.ArchiveStatus
		want   bool
	}{
		{
			name: "pending live download blocks another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				TaskVideoDownload: utils.Pending,
			},
			status: utils.ArchiveRunning,
			want:   true,
		},
		{
			name: "running live download blocks another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				TaskVideoDownload: utils.Running,
			},
			status: utils.ArchiveRunning,
			want:   true,
		},
		{
			name: "downloaded segment does not block another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				TaskVideoDownload: utils.Success,
			},
			status: utils.ArchiveRunning,
			want:   false,
		},
		{
			name: "failed metadata still blocks duplicate capture while download is pending",
			q: &ent.Queue{
				LiveArchive:              true,
				TaskVodDownloadThumbnail: utils.Failed,
				TaskVideoDownload:        utils.Pending,
			},
			status: utils.ArchiveRunning,
			want:   true,
		},
		{
			name: "failed create folder does not block recovery",
			q: &ent.Queue{
				LiveArchive:         true,
				TaskVodCreateFolder: utils.Failed,
				TaskVideoDownload:   utils.Pending,
			},
			status: utils.ArchiveRunning,
			want:   false,
		},
		{
			name: "completed archive does not block another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				TaskVideoDownload: utils.Running,
			},
			status: utils.ArchiveCompleted,
			want:   false,
		},
		{
			name: "regular vod queue is ignored",
			q: &ent.Queue{
				LiveArchive:       false,
				TaskVideoDownload: utils.Running,
			},
			status: utils.ArchiveRunning,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsActiveLiveCaptureQueue(tt.q, tt.status); got != tt.want {
				t.Fatalf("IsActiveLiveCaptureQueue() = %v, want %v", got, tt.want)
			}
		})
	}
}
