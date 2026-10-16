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
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
)

// counts is the waiting work of one app ID.
type counts struct {
	workflow uint32
	activity uint32
}

// watcher replays a WatchDemand stream into the waiting work per
// "<namespace>/<app ID>", as a client of the API would.
type watcher struct {
	lock      sync.Mutex
	apps      map[string]counts
	snapshots int
	messages  int
	errCh     chan error
}

// watch opens a WatchDemand stream and replays it in the background until
// the stream ends. It waits for the first message, which must be a snapshot.
func watch(t *testing.T, ctx context.Context, client schedulerv1pb.SchedulerClient) *watcher {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)

	stream, err := client.WatchDemand(ctx, new(schedulerv1pb.WatchDemandRequest))
	require.NoError(t, err)

	resp, err := stream.Recv()
	require.NoError(t, err)
	require.True(t, resp.GetSnapshot(), "first message must be a snapshot")

	w := &watcher{
		apps:  make(map[string]counts),
		errCh: make(chan error, 1),
	}
	w.apply(resp)

	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				w.errCh <- err
				return
			}
			w.apply(resp)
		}
	}()

	return w
}

func (w *watcher) apply(resp *schedulerv1pb.WatchDemandResponse) {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.messages++
	if resp.GetSnapshot() {
		w.snapshots++
		clear(w.apps)
	}

	for _, app := range resp.GetApps() {
		key := app.GetNamespace() + "/" + app.GetAppId()
		if app.GetWorkflowJobs() == 0 && app.GetActivityJobs() == 0 {
			delete(w.apps, key)
			continue
		}
		w.apps[key] = counts{workflow: app.GetWorkflowJobs(), activity: app.GetActivityJobs()}
	}
}

// state returns the waiting work per "<namespace>/<app ID>".
func (w *watcher) state() map[string]counts {
	w.lock.Lock()
	defer w.lock.Unlock()

	return maps.Clone(w.apps)
}

func (w *watcher) snapshotCount() int {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.snapshots
}

func (w *watcher) messageCount() int {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.messages
}

// sum adds the waiting work reported by every watcher.
func sum(watchers ...*watcher) map[string]counts {
	total := make(map[string]counts)
	for _, w := range watchers {
		for k, v := range w.state() {
			c := total[k]
			c.workflow += v.workflow
			c.activity += v.activity
			total[k] = c
		}
	}
	return total
}

// actorReminder is a job due now on the actor, created by createdBy.
func actorReminder(name, createdBy, actorType, actorID string) *schedulerv1pb.ScheduleJobRequest {
	return &schedulerv1pb.ScheduleJobRequest{
		Name: name,
		Job:  &schedulerv1pb.Job{DueTime: new(time.Now().Format(time.RFC3339))},
		Metadata: &schedulerv1pb.JobMetadata{
			Namespace: "default", AppId: createdBy,
			Target: &schedulerv1pb.JobTargetMetadata{
				Type: &schedulerv1pb.JobTargetMetadata_Actor{
					Actor: &schedulerv1pb.TargetActorReminder{Type: actorType, Id: actorID},
				},
			},
		},
	}
}

const (
	app1         = "default/app1"
	workflowType = "dapr.internal.default.app1.workflow"
	activityType = "dapr.internal.default.app1.activity"
)

// eventuallyState asserts the watchers eventually sum to exp.
func eventuallyState(t *testing.T, exp map[string]counts, watchers ...*watcher) {
	t.Helper()
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, exp, sum(watchers...))
	}, 20*time.Second, 10*time.Millisecond)
}
