package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/zibbp/ganymede/internal/database"
)

func TestUpstreamMigrationPreservesForkSchema(t *testing.T) {
	if os.Getenv("SKIP_SECRET_TESTS") == "true" {
		t.Skip("Skipping container-backed migration test")
	}

	for _, role := range []struct {
		name     string
		isWorker bool
	}{{"API starts first", false}, {"worker starts first", true}} {
		t.Run(role.name, func(t *testing.T) {
			ctx := t.Context()
			container, err := postgres.Run(ctx, "postgres:17-alpine",
				postgres.WithDatabase("ganymede"),
				postgres.WithUsername("ganymede"),
				postgres.WithPassword("ganymede"),
				testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(30*time.Second)),
			)
			require.NoError(t, err)
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				require.NoError(t, container.Terminate(cleanupCtx))
			})
			host, err := container.Host(ctx)
			require.NoError(t, err)
			port, err := container.MappedPort(ctx, "5432")
			require.NoError(t, err)
			connection := fmt.Sprintf("host=%s port=%s user=ganymede password=ganymede dbname=ganymede sslmode=disable", host, port.Port())
			db := database.NewDatabase(ctx, database.DatabaseConnectionInput{DBString: connection, IsWorker: true})
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			qualities := map[uuid.UUID]string{}
			for _, quality := range []string{"720p", "audio", "best"} {
				channel, err := db.Client.Channel.Create().SetName(quality).SetDisplayName(quality).
					SetImagePath("/tmp/migration-profile.png").Save(ctx)
				require.NoError(t, err)
				watched, err := db.Client.Live.Create().SetChannelID(channel.ID).SetVodResolution(quality).Save(ctx)
				require.NoError(t, err)
				expected := quality
				if quality == "audio" {
					expected = "best"
				}
				qualities[watched.ID] = expected
			}

			// Reproduce the fork schema before notes and separate clip quality existed.
			_, err = db.SQLDB.ExecContext(ctx, "ALTER TABLE lives DROP COLUMN clip_resolution")
			require.NoError(t, err)
			_, err = db.SQLDB.ExecContext(ctx, "ALTER TABLE vods DROP COLUMN notes")
			require.NoError(t, err)
			require.NoError(t, db.Close())
			db = database.NewDatabase(ctx, database.DatabaseConnectionInput{DBString: connection, IsWorker: role.isWorker})

			for id, quality := range qualities {
				watched, err := db.Client.Live.Get(ctx, id)
				require.NoError(t, err)
				require.Equal(t, quality, watched.ClipResolution)
			}
			var notesExist bool
			err = db.SQLDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'vods' AND column_name = 'notes')`).Scan(&notesExist)
			require.NoError(t, err)
			require.True(t, notesExist)
			var viewsCount int
			err = db.SQLDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'vods' AND column_name IN ('views', 'local_views')`).Scan(&viewsCount)
			require.NoError(t, err)
			require.Zero(t, viewsCount)

			// Explicit clip quality must survive the next process startup.
			for id := range qualities {
				_, err := db.Client.Live.UpdateOneID(id).SetClipResolution("360p").Save(ctx)
				require.NoError(t, err)
			}
			require.NoError(t, db.Close())
			db = database.NewDatabase(ctx, database.DatabaseConnectionInput{DBString: connection, IsWorker: !role.isWorker})
			for id := range qualities {
				watched, err := db.Client.Live.Get(ctx, id)
				require.NoError(t, err)
				require.Equal(t, "360p", watched.ClipResolution)
			}
		})
	}
}
