package queue

import (
	"testing"

	"github.com/zibbp/ganymede/ent"
	"github.com/zibbp/ganymede/internal/utils"
)

func TestIsActiveLiveCaptureQueue(t *testing.T) {
	tests := []struct {
		name string
		q    *ent.Queue
		want bool
	}{
		{
			name: "pending live download blocks another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				Processing:        true,
				TaskVideoDownload: utils.Pending,
			},
			want: true,
		},
		{
			name: "running live download blocks another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				Processing:        true,
				TaskVideoDownload: utils.Running,
			},
			want: true,
		},
		{
			name: "downloaded segment does not block another capture",
			q: &ent.Queue{
				LiveArchive:       true,
				Processing:        true,
				TaskVideoDownload: utils.Success,
			},
			want: false,
		},
		{
			name: "failed setup does not block recovery",
			q: &ent.Queue{
				LiveArchive:              true,
				Processing:               true,
				TaskVodDownloadThumbnail: utils.Failed,
				TaskVideoDownload:        utils.Pending,
			},
			want: false,
		},
		{
			name: "regular vod queue is ignored",
			q: &ent.Queue{
				LiveArchive:       false,
				Processing:        true,
				TaskVideoDownload: utils.Running,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsActiveLiveCaptureQueue(tt.q); got != tt.want {
				t.Fatalf("IsActiveLiveCaptureQueue() = %v, want %v", got, tt.want)
			}
		})
	}
}
