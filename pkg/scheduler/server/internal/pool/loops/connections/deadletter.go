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
	"slices"
	"strings"

	"github.com/diagridio/go-etcd-cron/api"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
	"github.com/dapr/dapr/pkg/scheduler/server/internal/pool/loops"
	"github.com/dapr/kit/events/loop"
)

// deadLetter is a stream subscribed to the triggers in its namespace which no
// other stream serves.
type deadLetter struct {
	loop   loop.Interface[loops.EventStream]
	filter *schedulerv1pb.WatchJobsDeadLetter
}

// addDeadLetter registers a dead-letter stream, returning the func which
// removes it and closes its loop.
func (c *connections) addDeadLetter(streamLoop loop.Interface[loops.EventStream], filter *schedulerv1pb.WatchJobsDeadLetter) context.CancelFunc {
	dl := &deadLetter{loop: streamLoop, filter: filter}
	c.deadLetters = append(c.deadLetters, dl)
	return func() {
		c.deadLetters = slices.DeleteFunc(c.deadLetters, func(d *deadLetter) bool {
			return d == dl
		})
		streamLoop.Close(new(loops.StreamShutdown))
	}
}

// undeliverable resolves a trigger which no stream serves: it is handed to a
// matching dead-letter stream when one is connected, or else answered
// undeliverable so the cron engine holds it until a stream serving its target
// connects.
func (c *connections) undeliverable(req *loops.TriggerRequest) {
	if streamLoop, ok := c.getDeadLetterLoop(req.Job.GetMetadata()); ok {
		streamLoop.Enqueue(req)
		return
	}

	req.ResultFn(api.TriggerResponseResult_UNDELIVERABLE)
}

// getDeadLetterLoop returns a dead-letter stream whose filter matches the job,
// round robin across the matching streams.
func (c *connections) getDeadLetterLoop(meta *schedulerv1pb.JobMetadata) (loop.Interface[loops.EventStream], bool) {
	n := uint64(len(c.deadLetters))
	for i := range n {
		dl := c.deadLetters[(c.nextDeadLetter+i)%n]
		if deadLetterMatches(dl.filter, meta) {
			c.nextDeadLetter += i + 1
			return dl.loop, true
		}
	}

	return nil, false
}

func deadLetterMatches(filter *schedulerv1pb.WatchJobsDeadLetter, meta *schedulerv1pb.JobMetadata) bool {
	switch t := meta.GetTarget().GetType().(type) {
	case *schedulerv1pb.JobTargetMetadata_Job:
		return matchAny(filter.GetAppIds(), meta.GetAppId())
	case *schedulerv1pb.JobTargetMetadata_Actor:
		return matchAny(filter.GetActorTypes(), t.Actor.GetType())
	default:
		return false
	}
}

func matchAny(patterns []string, s string) bool {
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		return match(pattern, s)
	})
}

// match reports whether s matches pattern in full, where '*' matches any run
// of characters, including none.
func match(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}

	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]

	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}

	return strings.HasSuffix(s, last)
}
