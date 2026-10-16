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

package connections

import (
	"context"
	"testing"

	"github.com/diagridio/go-etcd-cron/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apifake "github.com/diagridio/go-etcd-cron/tests/framework/fake"

	internalsv1pb "github.com/dapr/dapr/pkg/proto/internals/v1"
	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
	"github.com/dapr/dapr/pkg/scheduler/server/internal/pool/loops"
	"github.com/dapr/dapr/pkg/scheduler/server/internal/pool/loops/connections/store"
	loopfake "github.com/dapr/kit/events/loop/fake"
)

func TestMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern string
		s       string
		exp     bool
	}{
		{pattern: "abc", s: "abc", exp: true},
		{pattern: "abc", s: "abcd", exp: false},
		{pattern: "abc", s: "ab", exp: false},
		{pattern: "*", s: "", exp: true},
		{pattern: "*", s: "anything", exp: true},
		{pattern: "a*", s: "a", exp: true},
		{pattern: "a*", s: "ba", exp: false},
		{pattern: "*a", s: "ba", exp: true},
		{pattern: "*a", s: "ab", exp: false},
		{pattern: "dapr.internal.ns.*.workflow", s: "dapr.internal.ns.app1.workflow", exp: true},
		{pattern: "dapr.internal.ns.*.workflow", s: "dapr.internal.ns.my.app.workflow", exp: true},
		{pattern: "dapr.internal.ns.*.workflow", s: "dapr.internal.ns.app1.activity", exp: false},
		{pattern: "dapr.internal.ns.*.workflow", s: "dapr.internal.ns2.app1.workflow", exp: false},
		{pattern: "dapr.internal.ns.*.workflow", s: "dapr.internal.ns.workflow", exp: false},
		{pattern: "a*b*c", s: "abc", exp: true},
		{pattern: "a*b*c", s: "axxbyyc", exp: true},
		{pattern: "a*b*c", s: "acb", exp: false},
		{pattern: "a*bc*bc", s: "abcbc", exp: true},
		{pattern: "a*a", s: "a", exp: false},
	}

	for _, test := range tests {
		assert.Equal(t, test.exp, match(test.pattern, test.s), "match(%q, %q)", test.pattern, test.s)
	}
}

func TestDeadLetterRouting(t *testing.T) {
	t.Parallel()

	actorJob := func(actorType string) *loops.TriggerRequest {
		return &loops.TriggerRequest{Job: &internalsv1pb.JobEvent{Metadata: &schedulerv1pb.JobMetadata{
			Namespace: "ns", AppId: "app1",
			Target: &schedulerv1pb.JobTargetMetadata{
				Type: &schedulerv1pb.JobTargetMetadata_Actor{
					Actor: &schedulerv1pb.TargetActorReminder{Type: actorType, Id: "id"},
				},
			},
		}}}
	}
	appJob := func(appID string) *loops.TriggerRequest {
		return &loops.TriggerRequest{Job: &internalsv1pb.JobEvent{Metadata: &schedulerv1pb.JobMetadata{
			Namespace: "ns", AppId: appID,
			Target: &schedulerv1pb.JobTargetMetadata{Type: new(schedulerv1pb.JobTargetMetadata_Job)},
		}}}
	}

	type recorder struct {
		loop   *loopfake.Fake[loops.EventStream]
		got    []*loops.TriggerRequest
		closed bool
	}
	newRecorder := func() *recorder {
		r := new(recorder)
		r.loop = loopfake.New[loops.EventStream]().
			WithEnqueue(func(e loops.EventStream) {
				r.got = append(r.got, e.(*loops.TriggerRequest))
			}).
			WithClose(func(loops.EventStream) { r.closed = true })
		return r
	}

	// trigger sends req through the connections loop handler, returning the
	// result it was resolved with by the loop itself, if any.
	trigger := func(c *connections, req *loops.TriggerRequest) *api.TriggerResponseResult {
		var result *api.TriggerResponseResult
		req.ResultFn = func(r api.TriggerResponseResult) { result = &r }
		c.handleTriggerRequest(req)
		return result
	}

	newConns := func() *connections {
		return &connections{
			streamPool:       store.New(),
			concurrencyGates: make(map[string]*concurrencyGate),
		}
	}

	filter := &schedulerv1pb.WatchJobsDeadLetter{
		ActorTypes: []string{"dapr.internal.ns.*.workflow"},
		AppIds:     []string{"app1"},
	}

	t.Run("matching triggers with no host go to the dead-letter stream", func(t *testing.T) {
		t.Parallel()
		c := newConns()
		dl := newRecorder()
		c.addDeadLetter(dl.loop, filter)

		req1 := actorJob("dapr.internal.ns.app1.workflow")
		req2 := appJob("app1")
		assert.Nil(t, trigger(c, req1))
		assert.Nil(t, trigger(c, req2))
		assert.Equal(t, []*loops.TriggerRequest{req1, req2}, dl.got)
	})

	t.Run("non-matching triggers are undeliverable", func(t *testing.T) {
		t.Parallel()
		c := newConns()
		dl := newRecorder()
		c.addDeadLetter(dl.loop, filter)

		for _, req := range []*loops.TriggerRequest{
			actorJob("dapr.internal.ns.app1.activity"),
			actorJob("myactor"),
			appJob("app2"),
		} {
			res := trigger(c, req)
			require.NotNil(t, res)
			assert.Equal(t, api.TriggerResponseResult_UNDELIVERABLE, *res)
		}
		assert.Empty(t, dl.got)
	})

	t.Run("a serving host takes precedence over the dead-letter stream", func(t *testing.T) {
		t.Parallel()
		c := newConns()
		dl := newRecorder()
		c.addDeadLetter(dl.loop, filter)
		host := newRecorder()
		c.streamPool.Add(store.Options{
			Loop:       host.loop,
			ActorTypes: []string{"dapr.internal.ns.app1.workflow"},
		})

		req := actorJob("dapr.internal.ns.app1.workflow")
		assert.Nil(t, trigger(c, req))
		assert.Equal(t, []*loops.TriggerRequest{req}, host.got)
		assert.Empty(t, dl.got)
	})

	t.Run("triggers round robin across matching dead-letter streams", func(t *testing.T) {
		t.Parallel()
		c := newConns()
		other := newRecorder()
		c.addDeadLetter(other.loop, &schedulerv1pb.WatchJobsDeadLetter{AppIds: []string{"app9"}})
		dl1, dl2 := newRecorder(), newRecorder()
		c.addDeadLetter(dl1.loop, filter)
		c.addDeadLetter(dl2.loop, filter)

		for range 4 {
			assert.Nil(t, trigger(c, appJob("app1")))
		}
		assert.Len(t, dl1.got, 2)
		assert.Len(t, dl2.got, 2)
		assert.Empty(t, other.got)
	})

	t.Run("a removed dead-letter stream is closed and no longer receives", func(t *testing.T) {
		t.Parallel()
		c := newConns()
		dl := newRecorder()
		cancel := c.addDeadLetter(dl.loop, filter)
		cancel()
		assert.True(t, dl.closed)
		assert.Empty(t, c.deadLetters)

		res := trigger(c, appJob("app1"))
		require.NotNil(t, res)
		assert.Equal(t, api.TriggerResponseResult_UNDELIVERABLE, *res)
		assert.Empty(t, dl.got)
	})
}

func TestDeadLetterPrefixes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"actorreminder||ns||dapr.internal.ns.",
		"actorreminder||ns||myactor||",
		"app||ns||",
		"app||ns||app1||",
	}, deadLetterPrefixes("ns", &schedulerv1pb.WatchJobsDeadLetter{
		ActorTypes: []string{"dapr.internal.ns.*.workflow", "myactor", "dapr.internal.ns.*.activity"},
		AppIds:     []string{"app1", "*"},
	}))
}

func TestReplayDeadLetters(t *testing.T) {
	t.Parallel()

	var calls []string
	cron := apifake.New().WithDeliverablePrefixes(func(_ context.Context, prefixes ...string) (context.CancelCauseFunc, error) {
		calls = append(calls, "deliverable")
		assert.Equal(t, []string{"actorreminder||ns||dapr.internal.ns."}, prefixes)
		return func(error) { calls = append(calls, "undeliverable") }, nil
	})

	c := &connections{cron: cron}
	c.replayDeadLetters(t.Context(), "ns", &schedulerv1pb.WatchJobsDeadLetter{
		ActorTypes: []string{"dapr.internal.ns.*.workflow"},
	})

	// The prefixes are released at once, never held deliverable.
	assert.Equal(t, []string{"deliverable", "undeliverable"}, calls)
}
