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

// authz tests that with mTLS only the identities listed in
// --demand-watcher-ids may watch demand.
type authz struct {
	sentry    *sentry.Sentry
	scheduler *scheduler.Scheduler
}

func (a *authz) Setup(t *testing.T) []framework.Option {
	a.sentry = sentry.New(t)
	a.scheduler = scheduler.New(t,
		scheduler.WithSentry(a.sentry),
		scheduler.WithID("dapr-scheduler-server-0"),
		scheduler.WithDemandWatcherIDs("dapr-system/controller"),
	)

	return []framework.Option{
		framework.WithProcesses(a.sentry, a.scheduler),
	}
}

func (a *authz) Run(t *testing.T, ctx context.Context) {
	a.sentry.WaitUntilRunning(t, ctx)
	a.scheduler.WaitUntilRunning(t, ctx)

	denied := map[string]schedulerv1pb.SchedulerClient{
		"an app ID not listed":                 a.scheduler.ClientMTLSNS(t, ctx, "dapr-system", "app1"),
		"a listed app ID in another namespace": a.scheduler.ClientMTLSNS(t, ctx, "default", "controller"),
	}
	for name, client := range denied {
		t.Run(name+" is denied", func(t *testing.T) {
			stream, err := client.WatchDemand(ctx, new(schedulerv1pb.WatchDemandRequest))
			require.NoError(t, err)
			_, err = stream.Recv()
			require.Error(t, err)
			assert.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
		})
	}

	t.Run("a listed identity is allowed", func(t *testing.T) {
		w := watch(t, ctx, a.scheduler.ClientMTLSNS(t, ctx, "dapr-system", "controller"))

		appClient := a.scheduler.ClientMTLSNS(t, ctx, "default", "app1")
		_, err := appClient.ScheduleJob(ctx, actorReminder("start", "app1", workflowType, "wf1"))
		require.NoError(t, err)
		eventuallyState(t, map[string]counts{app1: {workflow: 1}}, w)
	})
}
