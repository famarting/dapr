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
	suite.Register(new(routing))
}

// routing tests that a trigger no host serves reaches a matching dead-letter
// consumer, that the consumer answering undeliverable leaves the job waiting
// for a host, and that a host connecting later receives it instead of the
// consumer.
type routing struct {
	scheduler *scheduler.Scheduler
}

func (r *routing) Setup(t *testing.T) []framework.Option {
	r.scheduler = scheduler.New(t, scheduler.WithDeadLetterAppIDs("dlq"))

	return []framework.Option{
		framework.WithProcesses(r.scheduler),
	}
}

func (r *routing) Run(t *testing.T, ctx context.Context) {
	r.scheduler.WaitUntilRunning(t, ctx)

	const (
		workflowType = "dapr.internal.default.app1.workflow"
		activityType = "dapr.internal.default.app1.activity"
	)

	var undeliverable atomic.Value
	undeliverable.Store(schedulerv1pb.WatchJobsRequestResultStatus_UNDELIVERABLE)
	consumer := r.scheduler.WatchJobs(t, ctx, deadLetterInitial(), &undeliverable)

	client := r.scheduler.Client(t, ctx)

	// Neither matches the filter, so both are held waiting for a host without
	// reaching the consumer.
	_, err := client.ScheduleJob(ctx, r.scheduler.JobNowActor("activity", "default", "app1", activityType, "1"))
	require.NoError(t, err)
	_, err = client.ScheduleJob(ctx, r.scheduler.JobNowJob("appjob", "default", "app1"))
	require.NoError(t, err)

	_, err = client.ScheduleJob(ctx, jobSoon(r.scheduler, "workflow", workflowType))
	require.NoError(t, err)

	select {
	case name := <-consumer:
		assert.Equal(t, "workflow", name)
	case <-time.After(time.Second * 10):
		require.Fail(t, "timed out waiting for the dead-letter trigger")
	}

	// The consumer answered undeliverable, so the job waits for a host and is
	// not retriggered to the consumer.
	expectNone(t, consumer, time.Second*3)

	workflowHost := r.scheduler.WatchJobsSuccess(t, ctx, &schedulerv1pb.WatchJobsRequestInitial{
		AppId:          "app1",
		Namespace:      "default",
		ActorTypes:     []string{workflowType},
		AcceptJobTypes: []schedulerv1pb.JobTargetType{schedulerv1pb.JobTargetType_JOB_TARGET_TYPE_ACTOR_REMINDER},
	})
	select {
	case name := <-workflowHost:
		assert.Equal(t, "workflow", name)
	case <-time.After(time.Second * 10):
		require.Fail(t, "timed out waiting for the host to receive the waiting job")
	}

	// The jobs which did not match were held for their hosts too.
	activityHost := r.scheduler.WatchJobsSuccess(t, ctx, &schedulerv1pb.WatchJobsRequestInitial{
		AppId:      "app1",
		Namespace:  "default",
		ActorTypes: []string{activityType},
	})
	got := make([]string, 0, 2)
	for range 2 {
		select {
		case name := <-activityHost:
			got = append(got, name)
		case <-time.After(time.Second * 10):
			require.Fail(t, "timed out waiting for the host to receive the waiting jobs")
		}
	}
	assert.ElementsMatch(t, []string{"activity", "appjob"}, got)

	expectNone(t, consumer, time.Second*3)
	expectNone(t, workflowHost, 0)
}

// deadLetterInitial is the initial request of a dead-letter consumer for the
// workflows of every app in the default namespace.
func deadLetterInitial() *schedulerv1pb.WatchJobsRequestInitial {
	return &schedulerv1pb.WatchJobsRequestInitial{
		AppId:     "dlq",
		Namespace: "default",
		DeadLetter: &schedulerv1pb.WatchJobsDeadLetter{
			ActorTypes: []string{"dapr.internal.default.*.workflow"},
		},
	}
}

// jobSoon returns a workflow job of app1 due in a few seconds, leaving time
// for a stream opened just before to be registered.
func jobSoon(s *scheduler.Scheduler, name, actorType string) *schedulerv1pb.ScheduleJobRequest {
	req := s.JobNowActor(name, "default", "app1", actorType, "1")
	req.Job.DueTime = new(time.Now().Add(time.Second * 3).Format(time.RFC3339))
	return req
}

func expectNone(t *testing.T, ch <-chan string, wait time.Duration) {
	t.Helper()

	select {
	case name := <-ch:
		assert.Failf(t, "unexpected trigger", "received %q", name)
	case <-time.After(wait):
	}
}
