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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	clocktesting "k8s.io/utils/clock/testing"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
)

const (
	testAppID = "app1"

	wf1  = "actorreminder||default||dapr.internal.default.app1.workflow||abc||start"
	wf2  = "actorreminder||default||dapr.internal.default.app1.workflow||def||timer-0"
	act1 = "actorreminder||default||dapr.internal.default.app1.activity||abc::0::1||run-activity"
	wf3  = "actorreminder||other||dapr.internal.other.app2.workflow||xyz||start"
)

func Test_parseJobName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name    string
		expKey  appKey
		expKind kind
		expOK   bool
	}{
		"workflow reminder": {
			name:    wf1,
			expKey:  appKey{namespace: "default", appID: testAppID},
			expKind: kindWorkflow,
			expOK:   true,
		},
		"activity reminder": {
			name:    act1,
			expKey:  appKey{namespace: "default", appID: testAppID},
			expKind: kindActivity,
			expOK:   true,
		},
		"namespace with dots": {
			name:    "actorreminder||my.ns||dapr.internal.my.ns.app1.workflow||abc||start",
			expKey:  appKey{namespace: "my.ns", appID: testAppID},
			expKind: kindWorkflow,
			expOK:   true,
		},
		"actor id and name containing ||": {
			name:    "actorreminder||default||dapr.internal.default.app1.activity||a||b||c||d",
			expKey:  appKey{namespace: "default", appID: testAppID},
			expKind: kindActivity,
			expOK:   true,
		},
		"app job": {
			name: "app||default||app1||dapr.internal.default.app1.workflow||start",
		},
		"ordinary actor type": {
			name: "actorreminder||default||myactor||abc||start",
		},
		"other internal actor type": {
			name: "actorreminder||default||dapr.internal.default.app1.executor||abc||start",
		},
		"internal actor type of another namespace": {
			name: "actorreminder||default||dapr.internal.other.app1.workflow||abc||start",
		},
		"workflow suffix not ending the actor type": {
			name: "actorreminder||default||dapr.internal.default.app1.workflowx||abc||start",
		},
		"workflow actor type with no actor id": {
			name: "actorreminder||default||dapr.internal.default.app1.workflow",
		},
		"empty app ID": {
			name: "actorreminder||default||dapr.internal.default..workflow||abc||start",
		},
		"empty namespace": {
			name: "actorreminder||||dapr.internal..app1.workflow||abc||start",
		},
		"empty": {},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, kind, ok := parseJobName(test.name)
			assert.Equal(t, test.expOK, ok)
			assert.Equal(t, test.expKey, key)
			assert.Equal(t, test.expKind, kind)
		})
	}
}

// watch starts a watcher on d, returning the channel of messages it sends.
func watch(t *testing.T, d *Demand) <-chan *schedulerv1pb.WatchDemandResponse {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	respCh := make(chan *schedulerv1pb.WatchDemandResponse, 10)
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Watch(ctx, func(resp *schedulerv1pb.WatchDemandResponse) error {
			respCh <- resp
			return nil
		})
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			require.Fail(t, "timed out waiting for watcher to stop")
		}
	})

	return respCh
}

func recv(t *testing.T, respCh <-chan *schedulerv1pb.WatchDemandResponse) *schedulerv1pb.WatchDemandResponse {
	t.Helper()
	select {
	case resp := <-respCh:
		return resp
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for message")
		return nil
	}
}

func noRecv(t *testing.T, respCh <-chan *schedulerv1pb.WatchDemandResponse) {
	t.Helper()
	select {
	case resp := <-respCh:
		require.Fail(t, "unexpected message", "%v", resp)
	case <-time.After(100 * time.Millisecond):
	}
}

// step waits for the watcher to wait on its coalesce window, then ends it.
func step(t *testing.T, clock *clocktesting.FakeClock) {
	t.Helper()
	assert.Eventually(t, clock.HasWaiters, 5*time.Second, time.Millisecond)
	clock.Step(DefaultCoalesceWindow)
}

func assertResp(t *testing.T, exp, got *schedulerv1pb.WatchDemandResponse) {
	t.Helper()
	assert.Truef(t, proto.Equal(sorted(exp), sorted(got)), "expected %v, got %v", exp, got)
}

func sorted(resp *schedulerv1pb.WatchDemandResponse) *schedulerv1pb.WatchDemandResponse {
	resp = proto.Clone(resp).(*schedulerv1pb.WatchDemandResponse)
	slices.SortFunc(resp.GetApps(), func(a, b *schedulerv1pb.AppDemand) int {
		return strings.Compare(a.GetNamespace()+"/"+a.GetAppId(), b.GetNamespace()+"/"+b.GetAppId())
	})
	return resp
}

func Test_Demand(t *testing.T) {
	t.Parallel()

	start := time.Unix(1000, 0)

	t.Run("first message is a snapshot of the current state", func(t *testing.T) {
		t.Parallel()

		clock := clocktesting.NewFakeClock(start)
		d := New(Options{Clock: clock})

		d.Staged(wf1)
		clock.Step(time.Second)
		d.Staged(wf2)
		d.Staged(act1)
		d.Staged(wf3)

		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Snapshot: true,
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 2, ActivityJobs: 1, OldestWaiting: timestamppb.New(start)},
				{Namespace: "other", AppId: "app2", WorkflowJobs: 1, OldestWaiting: timestamppb.New(start.Add(time.Second))},
			},
		}, recv(t, watch(t, d)))
	})

	t.Run("first message is sent when there is no demand", func(t *testing.T) {
		t.Parallel()

		d := New(Options{Clock: clocktesting.NewFakeClock(start)})
		assertResp(t, &schedulerv1pb.WatchDemandResponse{Snapshot: true, Apps: []*schedulerv1pb.AppDemand{}}, recv(t, watch(t, d)))
	})

	t.Run("other jobs are ignored", func(t *testing.T) {
		t.Parallel()

		clock := clocktesting.NewFakeClock(start)
		d := New(Options{Clock: clock})
		respCh := watch(t, d)
		recv(t, respCh)

		d.Staged("app||default||app1||job")
		d.Staged("actorreminder||default||myactor||abc||start")
		d.Unstaged([]string{"app||default||app1||job", wf1})
		assert.Empty(t, d.apps)
		noRecv(t, respCh)
	})

	t.Run("changes within the coalesce window are sent as one delta", func(t *testing.T) {
		t.Parallel()

		clock := clocktesting.NewFakeClock(start)
		d := New(Options{Clock: clock})
		respCh := watch(t, d)
		recv(t, respCh)

		d.Staged(wf1)
		d.Staged(wf1)
		d.Staged(act1)
		d.Staged(wf3)
		d.Unstaged([]string{wf3})
		noRecv(t, respCh)
		step(t, clock)

		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 1, ActivityJobs: 1, OldestWaiting: timestamppb.New(start)},
				{Namespace: "other", AppId: "app2"},
			},
		}, recv(t, respCh))

		// Only the app IDs which changed are sent.
		clock.Step(time.Second)
		d.Staged(wf2)
		step(t, clock)
		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 2, ActivityJobs: 1, OldestWaiting: timestamppb.New(start)},
			},
		}, recv(t, respCh))

		// Removing the oldest job moves the oldest waiting time forward.
		d.Unstaged([]string{wf1, act1, "actorreminder||default||dapr.internal.default.app1.workflow||unknown||x"})
		step(t, clock)
		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 1, OldestWaiting: timestamppb.New(start.Add(DefaultCoalesceWindow + time.Second))},
			},
		}, recv(t, respCh))

		// Removing the last job sends zero counts.
		d.Unstaged([]string{wf2})
		step(t, clock)
		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Apps: []*schedulerv1pb.AppDemand{{Namespace: "default", AppId: testAppID}},
		}, recv(t, respCh))
		assert.Empty(t, d.apps)
	})

	t.Run("a reset sends a snapshot replacing the previous state", func(t *testing.T) {
		t.Parallel()

		clock := clocktesting.NewFakeClock(start)
		d := New(Options{Clock: clock})
		respCh := watch(t, d)
		recv(t, respCh)

		d.Staged(wf1)
		d.Staged(wf3)
		step(t, clock)
		recv(t, respCh)

		// A job staged again after a reset waits from when it was staged
		// again.
		d.Staged(act1)
		d.Reset()
		d.Staged(wf3)
		step(t, clock)

		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Snapshot: true,
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "other", AppId: "app2", WorkflowJobs: 1, OldestWaiting: timestamppb.New(start.Add(DefaultCoalesceWindow))},
			},
		}, recv(t, respCh))
		noRecv(t, respCh)
	})

	t.Run("every watcher receives the changes", func(t *testing.T) {
		t.Parallel()

		clock := clocktesting.NewFakeClock(start)
		d := New(Options{Clock: clock})
		respCh1 := watch(t, d)
		recv(t, respCh1)

		d.Staged(wf1)
		respCh2 := watch(t, d)
		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Snapshot: true,
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 1, OldestWaiting: timestamppb.New(start)},
			},
		}, recv(t, respCh2))

		step(t, clock)
		assertResp(t, &schedulerv1pb.WatchDemandResponse{
			Apps: []*schedulerv1pb.AppDemand{
				{Namespace: "default", AppId: testAppID, WorkflowJobs: 1, OldestWaiting: timestamppb.New(start)},
			},
		}, recv(t, respCh1))
	})

	t.Run("a send error ends the watch and removes the watcher", func(t *testing.T) {
		t.Parallel()

		d := New(Options{Clock: clocktesting.NewFakeClock(start)})
		err := d.Watch(t.Context(), func(*schedulerv1pb.WatchDemandResponse) error {
			return errors.New("send failed")
		})
		require.EqualError(t, err, "send failed")
		assert.Empty(t, d.watchers)
	})
}
