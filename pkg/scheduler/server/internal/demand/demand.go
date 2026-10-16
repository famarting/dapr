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

// Package demand tracks the workflow and activity actor reminders which are
// waiting on this scheduler because no connected sidecar hosts their actor
// type, and streams the per app ID totals to watchers.
package demand

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/utils/clock"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
)

// DefaultCoalesceWindow is how long a watcher gathers changes before it sends
// them, so a burst of jobs becoming staged at once reaches it as one message.
const DefaultCoalesceWindow = 250 * time.Millisecond

type Options struct {
	// CoalesceWindow overrides DefaultCoalesceWindow.
	CoalesceWindow time.Duration

	// Clock overrides the real clock, for testing.
	Clock clock.Clock
}

// Demand is the go-etcd-cron StagingObserver of the scheduler. It keeps the
// staged workflow and activity actor reminders of each app ID, ignoring every
// other job, and streams the totals to watchers.
//
// The observer methods are called by the cron engine with its staging lock
// held, so they only update the state and wake the watchers; the watchers
// build and send their messages on their own goroutines.
type Demand struct {
	window time.Duration
	clock  clock.Clock

	lock     sync.Mutex
	apps     map[appKey]*app
	watchers map[*watcher]struct{}
}

type appKey struct {
	namespace string
	appID     string
}

type kind uint8

const (
	kindWorkflow kind = iota
	kindActivity
)

type job struct {
	kind     kind
	stagedAt time.Time
}

type app struct {
	jobs     map[string]job
	workflow uint32
	activity uint32
}

// watcher is the pending state of one stream: whether it needs the complete
// state, and otherwise the app IDs which changed since it last sent.
type watcher struct {
	notifyCh chan struct{}
	snapshot bool
	dirty    map[appKey]struct{}
}

func New(opts Options) *Demand {
	window := opts.CoalesceWindow
	if window <= 0 {
		window = DefaultCoalesceWindow
	}

	clk := opts.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}

	return &Demand{
		window:   window,
		clock:    clk,
		apps:     make(map[appKey]*app),
		watchers: make(map[*watcher]struct{}),
	}
}

// Staged implements go-etcd-cron's api.StagingObserver.
func (d *Demand) Staged(name string) {
	key, k, ok := parseJobName(name)
	if !ok {
		return
	}

	d.lock.Lock()
	defer d.lock.Unlock()

	a, ok := d.apps[key]
	if !ok {
		a = &app{jobs: make(map[string]job)}
		d.apps[key] = a
	}

	if _, ok = a.jobs[name]; ok {
		return
	}

	a.jobs[name] = job{kind: k, stagedAt: d.clock.Now()}
	switch k {
	case kindWorkflow:
		a.workflow++
	case kindActivity:
		a.activity++
	}

	d.markDirty(key)
}

// Unstaged implements go-etcd-cron's api.StagingObserver.
func (d *Demand) Unstaged(names []string) {
	d.lock.Lock()
	defer d.lock.Unlock()

	for _, name := range names {
		key, _, ok := parseJobName(name)
		if !ok {
			continue
		}

		a, ok := d.apps[key]
		if !ok {
			continue
		}

		j, ok := a.jobs[name]
		if !ok {
			continue
		}

		delete(a.jobs, name)
		switch j.kind {
		case kindWorkflow:
			a.workflow--
		case kindActivity:
			a.activity--
		}

		if len(a.jobs) == 0 {
			delete(d.apps, key)
		}

		d.markDirty(key)
	}
}

// Reset implements go-etcd-cron's api.StagingObserver. Every watcher is sent
// the complete state next.
func (d *Demand) Reset() {
	d.lock.Lock()
	defer d.lock.Unlock()

	clear(d.apps)

	for w := range d.watchers {
		w.snapshot = true
		clear(w.dirty)
		w.notify()
	}
}

// Watch sends the complete state to send, then the changes as they happen,
// until ctx is done or send returns an error.
func (d *Demand) Watch(ctx context.Context, send func(*schedulerv1pb.WatchDemandResponse) error) error {
	w := &watcher{
		notifyCh: make(chan struct{}, 1),
		snapshot: true,
		dirty:    make(map[appKey]struct{}),
	}

	d.lock.Lock()
	d.watchers[w] = struct{}{}
	d.lock.Unlock()

	defer func() {
		d.lock.Lock()
		delete(d.watchers, w)
		d.lock.Unlock()
	}()

	// The first message is sent straight away, later ones after gathering
	// changes for the coalesce window.
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-w.notifyCh:
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-d.clock.After(d.window):
			}
		}
		first = false

		if resp := d.collect(w); resp != nil {
			if err := send(resp); err != nil {
				return err
			}
		}
	}
}

// collect builds the next message for the watcher from its pending state, and
// clears it. Returns nil when there is nothing to send.
func (d *Demand) collect(w *watcher) *schedulerv1pb.WatchDemandResponse {
	d.lock.Lock()
	defer d.lock.Unlock()

	// A notification which arrived while the previous message was being
	// built is covered by this one.
	select {
	case <-w.notifyCh:
	default:
	}

	if w.snapshot {
		w.snapshot = false
		clear(w.dirty)

		apps := make([]*schedulerv1pb.AppDemand, 0, len(d.apps))
		for key, a := range d.apps {
			apps = append(apps, a.toProto(key))
		}

		return &schedulerv1pb.WatchDemandResponse{Snapshot: true, Apps: apps}
	}

	if len(w.dirty) == 0 {
		return nil
	}

	apps := make([]*schedulerv1pb.AppDemand, 0, len(w.dirty))
	for key := range w.dirty {
		a, ok := d.apps[key]
		if !ok {
			a = new(app)
		}
		apps = append(apps, a.toProto(key))
	}
	clear(w.dirty)

	return &schedulerv1pb.WatchDemandResponse{Apps: apps}
}

// markDirty records the app ID as changed for every watcher. Must be called
// with the lock held.
func (d *Demand) markDirty(key appKey) {
	for w := range d.watchers {
		if !w.snapshot {
			w.dirty[key] = struct{}{}
		}
		w.notify()
	}
}

func (w *watcher) notify() {
	select {
	case w.notifyCh <- struct{}{}:
	default:
	}
}

func (a *app) toProto(key appKey) *schedulerv1pb.AppDemand {
	resp := &schedulerv1pb.AppDemand{
		Namespace:    key.namespace,
		AppId:        key.appID,
		WorkflowJobs: a.workflow,
		ActivityJobs: a.activity,
	}

	var oldest time.Time
	for _, j := range a.jobs {
		if oldest.IsZero() || j.stagedAt.Before(oldest) {
			oldest = j.stagedAt
		}
	}
	if !oldest.IsZero() {
		resp.OldestWaiting = timestamppb.New(oldest)
	}

	return resp
}

const (
	actorReminderPrefix = "actorreminder||"
	internalTypePrefix  = "dapr.internal."
	workflowTypeSuffix  = "workflow||"
	activityTypeSuffix  = "activity||"
)

// parseJobName reports whether the scheduler job name is a reminder on a
// workflow or activity actor type, and if so the namespace and app ID which
// host that actor type.
//
// An actor reminder job is named
// "actorreminder||<namespace>||<actor type>||<actor id>||<name>", and the
// workflow and activity actor types are
// "dapr.internal.<namespace>.<app id>.workflow" and ".activity". The app ID
// is taken from the actor type, never from the job metadata, which holds the
// app ID that created the job. An app ID cannot contain a dot, so the type
// is unambiguous once its namespace is known.
func parseJobName(name string) (appKey, kind, bool) {
	rest, ok := strings.CutPrefix(name, actorReminderPrefix)
	if !ok {
		return appKey{}, 0, false
	}

	ns, rest, ok := strings.Cut(rest, "||")
	if !ok || ns == "" {
		return appKey{}, 0, false
	}

	rest, ok = strings.CutPrefix(rest, internalTypePrefix)
	if !ok {
		return appKey{}, 0, false
	}

	rest, ok = strings.CutPrefix(rest, ns)
	if !ok {
		return appKey{}, 0, false
	}

	rest, ok = strings.CutPrefix(rest, ".")
	if !ok {
		return appKey{}, 0, false
	}

	appID, rest, ok := strings.Cut(rest, ".")
	if !ok || appID == "" {
		return appKey{}, 0, false
	}

	var k kind
	switch {
	case strings.HasPrefix(rest, workflowTypeSuffix):
		k = kindWorkflow
	case strings.HasPrefix(rest, activityTypeSuffix):
		k = kindActivity
	default:
		return appKey{}, 0, false
	}

	return appKey{namespace: ns, appID: appID}, k, true
}
