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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
	"github.com/dapr/dapr/tests/integration/framework"
	"github.com/dapr/dapr/tests/integration/framework/process/scheduler"
	"github.com/dapr/dapr/tests/integration/framework/process/sentry"
	"github.com/dapr/dapr/tests/integration/suite"
)

func init() {
	suite.Register(new(authz))
}

// authz tests that only an allowed App ID may subscribe as a dead-letter
// consumer, only in its own namespace, and that the filter must declare no
// target of its own.
type authz struct {
	sentry    *sentry.Sentry
	scheduler *scheduler.Scheduler
}

func (a *authz) Setup(t *testing.T) []framework.Option {
	a.sentry = sentry.New(t)
	a.scheduler = scheduler.New(t,
		scheduler.WithSentry(a.sentry),
		scheduler.WithID("dapr-scheduler-server-0"),
		scheduler.WithDeadLetterAppIDs("dlq"),
	)

	return []framework.Option{
		framework.WithProcesses(a.sentry, a.scheduler),
	}
}

func (a *authz) Run(t *testing.T, ctx context.Context) {
	a.sentry.WaitUntilRunning(t, ctx)
	a.scheduler.WaitUntilRunning(t, ctx)

	const workflowType = "dapr.internal.default.app1.workflow"

	watch := func(t *testing.T, client schedulerv1pb.SchedulerClient, initial *schedulerv1pb.WatchJobsRequestInitial) schedulerv1pb.Scheduler_WatchJobsClient {
		t.Helper()
		wctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		stream, err := client.WatchJobs(wctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&schedulerv1pb.WatchJobsRequest{
			WatchJobRequestType: &schedulerv1pb.WatchJobsRequest_Initial{Initial: initial},
		}))
		return stream
	}

	expectCode := func(t *testing.T, client schedulerv1pb.SchedulerClient, initial *schedulerv1pb.WatchJobsRequestInitial, code codes.Code) {
		t.Helper()
		_, err := watch(t, client, initial).Recv()
		require.Error(t, err)
		assert.Equal(t, code, status.Code(err), "%v", err)
	}

	dlq := a.scheduler.ClientMTLSNS(t, ctx, "default", "dlq")

	t.Run("another namespace is rejected", func(t *testing.T) {
		initial := deadLetterInitial()
		initial.Namespace = "other"
		initial.DeadLetter.ActorTypes = []string{"dapr.internal.other.*.workflow"}
		expectCode(t, dlq, initial, codes.PermissionDenied)
	})

	t.Run("an app ID not allowed is rejected", func(t *testing.T) {
		initial := deadLetterInitial()
		initial.AppId = "app1"
		expectCode(t, a.scheduler.ClientMTLSNS(t, ctx, "default", "app1"), initial, codes.PermissionDenied)
	})

	t.Run("a dead-letter stream declaring a target is rejected", func(t *testing.T) {
		initial := deadLetterInitial()
		initial.ActorTypes = []string{workflowType}
		expectCode(t, dlq, initial, codes.InvalidArgument)
	})

	t.Run("an allowed app ID in its own namespace receives dead-letter triggers", func(t *testing.T) {
		stream := watch(t, dlq, deadLetterInitial())

		app1 := a.scheduler.ClientMTLSNS(t, ctx, "default", "app1")
		_, err := app1.ScheduleJob(ctx, jobSoon(a.scheduler, "workflow", workflowType))
		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		assert.Equal(t, "workflow", resp.GetName())
		assert.Equal(t, workflowType, resp.GetMetadata().GetTarget().GetActor().GetType())
		assert.Equal(t, "app1", resp.GetMetadata().GetAppId())
	})
}
