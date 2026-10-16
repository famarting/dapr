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
	suite.Register(new(consume))
}

// consume tests that a dead-letter consumer's success answer is
// authoritative: the trigger counts as delivered, so a one-shot job is done
// and a host connecting later does not receive it.
type consume struct {
	scheduler *scheduler.Scheduler
}

func (c *consume) Setup(t *testing.T) []framework.Option {
	c.scheduler = scheduler.New(t, scheduler.WithDeadLetterAppIDs("dlq"))

	return []framework.Option{
		framework.WithProcesses(c.scheduler),
	}
}

func (c *consume) Run(t *testing.T, ctx context.Context) {
	c.scheduler.WaitUntilRunning(t, ctx)

	const workflowType = "dapr.internal.default.app1.workflow"

	consumer := c.scheduler.WatchJobsSuccess(t, ctx, deadLetterInitial())

	_, err := c.scheduler.Client(t, ctx).ScheduleJob(ctx, jobSoon(c.scheduler, "workflow", workflowType))
	require.NoError(t, err)

	select {
	case name := <-consumer:
		assert.Equal(t, "workflow", name)
	case <-time.After(time.Second * 10):
		require.Fail(t, "timed out waiting for the dead-letter trigger")
	}

	assert.EventuallyWithT(t, func(col *assert.CollectT) {
		assert.Empty(col, c.scheduler.ListJobActors(t, ctx, "default", "app1", workflowType, "1").GetJobs())
	}, time.Second*10, time.Millisecond*10)

	host := c.scheduler.WatchJobsSuccess(t, ctx, &schedulerv1pb.WatchJobsRequestInitial{
		AppId:      "app1",
		Namespace:  "default",
		ActorTypes: []string{workflowType},
	})
	expectNone(t, host, time.Second*3)
}
