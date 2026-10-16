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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/os"
	"github.com/dapr/dapr/tests/integration/framework/process/scheduler/cluster"
	"github.com/dapr/dapr/tests/integration/suite"
)

func init() {
	suite.Register(new(clustered))
}

// clustered tests that in a scheduler cluster each replica reports the
// waiting jobs of its own partition, so the replicas sum to every waiting
// job, including after a replica restarts and the partitions rebalance.
type clustered struct {
	cluster *cluster.Cluster
}

func (c *clustered) Setup(t *testing.T) []framework.Option {
	os.SkipWindows(t)

	c.cluster = cluster.New(t, cluster.WithCount(3))
	return []framework.Option{
		framework.WithProcesses(c.cluster),
	}
}

func (c *clustered) Run(t *testing.T, ctx context.Context) {
	c.cluster.WaitUntilRunning(t, ctx)

	watchers := make([]*watcher, 3)
	for i := range watchers {
		watchers[i] = watch(t, ctx, c.cluster.ClientN(t, ctx, i))
	}

	// Jobs are assigned to partitions at random, so use enough of them that
	// every replica likely owns some.
	const workflows, activities = 20, 10
	client := c.cluster.Client(t, ctx)
	for i := range workflows {
		_, err := client.ScheduleJob(ctx, actorReminder("start", "app1", workflowType, "wf"+strconv.Itoa(i)))
		require.NoError(t, err)
	}
	for i := range activities {
		_, err := client.ScheduleJob(ctx, actorReminder("run-activity", "app1", activityType, "act"+strconv.Itoa(i)))
		require.NoError(t, err)
	}

	exp := map[string]counts{app1: {workflow: workflows, activity: activities}}
	eventuallyState(t, exp, watchers...)

	var reporting int
	for _, w := range watchers {
		if len(w.state()) > 0 {
			reporting++
		}
	}
	assert.Greater(t, reporting, 1, "jobs should be spread over more than one replica")

	// Restarting a replica rebalances the partitions twice: when it leaves and
	// when it rejoins. Each surviving replica drops its waiting jobs and
	// reports a snapshot every time, then reports the jobs of its new
	// partition as they wait again.
	const restarted = 1
	before := make([]int, 3)
	for i, w := range watchers {
		before[i] = w.snapshotCount()
	}

	c.cluster.SchedulerN(t, restarted).RestartGraceful(t, ctx)
	select {
	case <-watchers[restarted].errCh:
	case <-time.After(10 * time.Second):
		require.Fail(t, "stream to the restarted replica did not end")
	}

	c.cluster.WaitUntilRunning(t, ctx)
	watchers[restarted] = watch(t, ctx, c.cluster.ClientN(t, ctx, restarted))

	assert.EventuallyWithT(t, func(col *assert.CollectT) {
		for i, w := range watchers {
			if i != restarted {
				assert.Greater(col, w.snapshotCount(), before[i], "replica %d did not report a snapshot after the rebalance", i)
			}
		}
		assert.Equal(col, exp, sum(watchers...))
	}, 30*time.Second, 10*time.Millisecond)
}
