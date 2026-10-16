/*
Copyright 2026 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package demand

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dapr/dapr/pkg/actors/targets/workflow/common"
	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/process/scheduler"
	"github.com/dapr/dapr/tests/integration/suite"
)

func init() {
	suite.Register(new(basic))
}

// basic tests that WatchDemand counts the workflow and activity reminders
// waiting for a host, per the app ID which hosts their actor type, and only
// those.
type basic struct {
	scheduler *scheduler.Scheduler
}

func (b *basic) Setup(t *testing.T) []framework.Option {
	b.scheduler = scheduler.New(t)
	return []framework.Option{
		framework.WithProcesses(b.scheduler),
	}
}

func (b *basic) Run(t *testing.T, ctx context.Context) {
	b.scheduler.WaitUntilRunning(t, ctx)

	client := b.scheduler.Client(t, ctx)
	w := watch(t, ctx, client)
	assert.Empty(t, w.state())

	// Jobs which are not workflow or activity reminders are not counted,
	// although they wait for a host all the same.
	_, err := client.ScheduleJob(ctx, b.scheduler.JobNowJob("job", "default", "app1"))
	require.NoError(t, err)
	_, err = client.ScheduleJob(ctx, actorReminder("reminder", "app1", "myactor", "a"))
	require.NoError(t, err)
	_, err = client.ScheduleJob(ctx, actorReminder("reminder", "app1", "dapr.internal.default.app1.executor", "a"))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return b.scheduler.JobKeyCount(t, ctx, "") == 3
	}, 10*time.Second, 10*time.Millisecond)
	time.Sleep(2 * time.Second)
	assert.Empty(t, w.state())
	assert.Equal(t, 1, w.messageCount(), "uncounted jobs must not send updates")

	t.Run("a workflow reminder with no host is counted", func(t *testing.T) {
		_, err := client.ScheduleJob(ctx, actorReminder("start", "app1", workflowType, "wf1"))
		require.NoError(t, err)
		eventuallyState(t, map[string]counts{app1: {workflow: 1}}, w)
	})

	t.Run("an activity reminder counts toward the app ID hosting it", func(t *testing.T) {
		_, err := client.ScheduleJob(ctx, actorReminder("run-activity", "app1", activityType, "wf1::0::1"))
		require.NoError(t, err)
		eventuallyState(t, map[string]counts{app1: {workflow: 1, activity: 1}}, w)
	})

	t.Run("a reminder counts toward the app ID hosting it, not the app ID which created it", func(t *testing.T) {
		// An activity host delivers an activity result to the workflow's app
		// by creating a reminder on that app's workflow actor.
		_, err := client.ScheduleJob(ctx, actorReminder(common.ReminderPrefixActivityResult+"abc", "app2", workflowType, "wf2"))
		require.NoError(t, err)
		eventuallyState(t, map[string]counts{app1: {workflow: 2, activity: 1}}, w)
	})

	t.Run("deleting a waiting job removes it", func(t *testing.T) {
		_, err := client.DeleteJob(ctx, &schedulerv1pb.DeleteJobRequest{
			Name:     "start",
			Metadata: actorReminder("start", "app1", workflowType, "wf1").GetMetadata(),
		})
		require.NoError(t, err)
		eventuallyState(t, map[string]counts{app1: {workflow: 1, activity: 1}}, w)
	})

	t.Run("a host for the actor types delivers the waiting jobs", func(t *testing.T) {
		triggered := b.scheduler.WatchJobsSuccess(t, ctx, &schedulerv1pb.WatchJobsRequestInitial{
			Namespace: "default", AppId: "app1",
			ActorTypes: []string{workflowType, activityType},
		})

		// The stream also serves app1's own app job.
		got := make([]string, 0, 3)
		for range 3 {
			select {
			case name := <-triggered:
				got = append(got, name)
			case <-time.After(10 * time.Second):
				require.Fail(t, "timed out waiting for the waiting jobs")
			}
		}
		assert.ElementsMatch(t, []string{"job", "run-activity", common.ReminderPrefixActivityResult + "abc"}, got)

		eventuallyState(t, map[string]counts{}, w)
	})
}
