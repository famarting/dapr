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

package deadletter

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/process/scheduler"
	"github.com/dapr/dapr/tests/integration/suite"
)

func init() {
	suite.Register(new(replay))
}

// replay tests that jobs already waiting for a host when a dead-letter
// consumer connects are replayed to it, again on every reconnect, and that a
// host connecting afterwards still receives them.
type replay struct {
	scheduler *scheduler.Scheduler
}

func (r *replay) Setup(t *testing.T) []framework.Option {
	r.scheduler = scheduler.New(t, scheduler.WithDeadLetterAppIDs("dlq"))

	return []framework.Option{
		framework.WithProcesses(r.scheduler),
	}
}

func (r *replay) Run(t *testing.T, ctx context.Context) {
	r.scheduler.WaitUntilRunning(t, ctx)

	client := r.scheduler.Client(t, ctx)
	_, err := client.ScheduleJob(ctx, r.scheduler.JobNowActor("wf1", "default", "app1", "dapr.internal.default.app1.workflow", "1"))
	require.NoError(t, err)
	_, err = client.ScheduleJob(ctx, r.scheduler.JobNowActor("wf2", "default", "app2", "dapr.internal.default.app2.workflow", "1"))
	require.NoError(t, err)

	// Both triggered with nothing connected, so both are waiting for a host.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		total := r.scheduler.MetricsWithLabels(t, ctx).Metrics["dapr_scheduler_jobs_undelivered_total"]
		assert.InDelta(c, 2.0, total["type=actor"], 0.01)
	}, time.Second*10, time.Millisecond*10)

	var undeliverable atomic.Value
	undeliverable.Store(schedulerv1pb.WatchJobsRequestResultStatus_UNDELIVERABLE)

	expectReplay := func(t *testing.T, ch <-chan string) {
		t.Helper()
		got := make([]string, 0, 2)
		for range 2 {
			select {
			case name := <-ch:
				got = append(got, name)
			case <-time.After(time.Second * 10):
				require.Fail(t, "timed out waiting for the waiting jobs to be replayed")
			}
		}
		assert.ElementsMatch(t, []string{"wf1", "wf2"}, got)
		expectNone(t, ch, time.Second*2)
	}

	cctx, cancel := context.WithCancel(ctx)
	expectReplay(t, r.scheduler.WatchJobs(t, cctx, deadLetterInitial(), &undeliverable))
	cancel()

	consumer := r.scheduler.WatchJobs(t, ctx, deadLetterInitial(), &undeliverable)
	expectReplay(t, consumer)

	host := r.scheduler.WatchJobsSuccess(t, ctx, &schedulerv1pb.WatchJobsRequestInitial{
		AppId:      "app1",
		Namespace:  "default",
		ActorTypes: []string{"dapr.internal.default.app1.workflow"},
	})
	select {
	case name := <-host:
		assert.Equal(t, "wf1", name)
	case <-time.After(time.Second * 10):
		require.Fail(t, "timed out waiting for the host to receive the waiting job")
	}

	expectNone(t, consumer, time.Second*2)
}
