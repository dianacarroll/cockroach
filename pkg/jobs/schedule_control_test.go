// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package jobs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobstest"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

func TestScheduleControl(t *testing.T) {
	defer leaktest.AfterTest(t)()
	th, cleanup := newTestHelper(t)
	defer cleanup()

	// Inject our test environment into schedule control execution via testing knobs.
	th.cfg.TestingKnobs.(*TestingKnobs).JobSchedulerEnv = th.env

	t.Run("non-existent", func(t *testing.T) {
		for _, command := range []string{
			"PAUSE SCHEDULE 123",
			"PAUSE SCHEDULES SELECT 123",
			"RESUME SCHEDULE 123",
			"RESUME SCHEDULES SELECT schedule_id FROM system.scheduled_jobs",
			"DROP SCHEDULE 123",
			"DROP SCHEDULES SELECT schedule_id FROM system.scheduled_jobs",
		} {
			t.Run(command, func(t *testing.T) {
				th.sqlDB.ExecRowsAffected(t, 0, command)
			})
		}
	})

	ctx := context.Background()

	var recurringNever string

	makeSchedule := func(name string, cron string) int64 {
		schedule := th.newScheduledJob(t, name, "sql")
		if cron != "" {
			require.NoError(t, schedule.SetSchedule(cron))
		}
		require.NoError(t, schedule.Create(ctx, th.cfg.InternalExecutor, nil))
		return schedule.ScheduleID()
	}

	t.Run("pause-one-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", "@daily")
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
	})

	t.Run("pause-one-off-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", recurringNever)
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
	})

	t.Run("cannot-resume-one-off-schedule", func(t *testing.T) {
		schedule := th.newScheduledJob(t, "test schedule", "select 42")
		require.NoError(t, schedule.Create(ctx, th.cfg.InternalExecutor, nil))

		th.sqlDB.ExpectErr(t, "cannot set next run for schedule",
			"RESUME SCHEDULE $1", schedule.ScheduleID())
	})

	t.Run("pause-and-resume-one-schedule", func(t *testing.T) {
		scheduleID := makeSchedule("one-schedule", "@daily")
		th.sqlDB.Exec(t, "PAUSE SCHEDULE $1", scheduleID)
		require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
		th.sqlDB.Exec(t, "RESUME SCHEDULE $1", scheduleID)

		schedule := th.loadSchedule(t, scheduleID)
		require.False(t, schedule.IsPaused())
	})

	t.Run("pause-resume-and-drop-many-schedules", func(t *testing.T) {
		var scheduleIDs []int64
		for i := 0; i < 10; i++ {
			scheduleIDs = append(
				scheduleIDs,
				makeSchedule(fmt.Sprintf("pause-resume-many-%d", i), "@daily"),
			)
		}

		querySchedules := "SELECT schedule_id FROM " + th.env.ScheduledJobsTableName() +
			" WHERE schedule_name LIKE 'pause-resume-many-%'"

		th.sqlDB.Exec(t, "PAUSE SCHEDULES "+querySchedules)

		for _, scheduleID := range scheduleIDs {
			require.True(t, th.loadSchedule(t, scheduleID).IsPaused())
		}

		th.sqlDB.Exec(t, "RESUME SCHEDULES "+querySchedules)

		for _, scheduleID := range scheduleIDs {
			require.False(t, th.loadSchedule(t, scheduleID).IsPaused())
		}

		th.sqlDB.Exec(t, "DROP SCHEDULES "+querySchedules)
		require.Equal(t, 0, len(th.sqlDB.QueryStr(t, querySchedules)))
	})
}

func TestJobsControlForSchedules(t *testing.T) {
	defer leaktest.AfterTest(t)()
	th, cleanup := newTestHelperForTables(t, jobstest.UseSystemTables)
	defer cleanup()

	registry := th.server.JobRegistry().(*Registry)
	blockResume := make(chan struct{})
	defer close(blockResume)

	// Our resume never completes any jobs, until this test completes.
	// As such, the job does not undergo usual job state transitions
	// (e.g. pause-request -> paused).
	RegisterConstructor(jobspb.TypeImport, func(job *Job, _ *cluster.Settings) Resumer {
		return FakeResumer{
			OnResume: func(_ context.Context, _ chan<- tree.Datums) error {
				<-blockResume
				return nil
			},
		}
	})

	record := Record{
		Description: "fake job",
		Username:    "test",
		Details:     jobspb.ImportDetails{},
		Progress:    jobspb.ImportProgress{},
	}

	const numJobs = 5

	// Create few jobs not started by any schedule.
	for i := 0; i < numJobs; i++ {
		require.NoError(t, registry.NewJob(record).Created(context.Background()))
	}

	var scheduleID int64 = 123

	for _, tc := range []struct {
		command      string
		numSchedules int
	}{
		{"pause", 1},
		{"resume", 1},
		{"cancel", 1},
		{"pause", 2},
		{"resume", 3},
		{"cancel", 4},
	} {
		schedulesStr := &strings.Builder{}
		for i := 0; i < tc.numSchedules; i++ {
			scheduleID++
			if schedulesStr.Len() > 0 {
				schedulesStr.WriteByte(',')
			}
			fmt.Fprintf(schedulesStr, "%d", scheduleID)

			for i := 0; i < numJobs; i++ {
				record.CreatedBy = &CreatedByInfo{
					Name: CreatedByScheduledJobs,
					ID:   scheduleID,
				}
				require.NoError(t, registry.NewJob(record).Created(context.Background()))

				if tc.command == "resume" {
					// Job has to be in paused state in order for it to be resumable;
					// Alas, because we don't actually run real jobs (see comment above),
					// We can't just pause the job (since it will stay in pause-requested state forever).
					// So, just force set job status to paused.
					th.sqlDB.Exec(t, "UPDATE system.jobs SET status=$1 WHERE id=$2", StatusPaused, scheduleID)
				}
			}
		}

		jobControl := tc.command + " JOBS FOR "
		if tc.numSchedules == 1 {
			jobControl += "SCHEDULE " + schedulesStr.String()
		} else {
			jobControl += fmt.Sprintf("SCHEDULES SELECT unnest(array[%s])", schedulesStr)
		}

		t.Run(jobControl, func(t *testing.T) {
			// Go through internal executor to execute job control command.
			// This correctly reports the number of effected rows.
			numEffected, err := th.cfg.InternalExecutor.ExecEx(
				context.Background(),
				"test-num-effected",
				nil,
				sqlbase.InternalExecutorSessionDataOverride{User: security.RootUser},
				jobControl,
			)
			require.NoError(t, err)
			require.Equal(t, numJobs*tc.numSchedules, numEffected)
		})
	}
}
