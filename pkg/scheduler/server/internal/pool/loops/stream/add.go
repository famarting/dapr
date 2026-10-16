/*
Copyright 2025 The Dapr Authors
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

package stream

import (
	"context"
	"slices"

	"github.com/diagridio/go-etcd-cron/api"

	schedulerv1pb "github.com/dapr/dapr/pkg/proto/scheduler/v1"
)

func handleAdd(ctx context.Context, cron api.Interface, add *schedulerv1pb.WatchJobsRequestInitial) (context.CancelCauseFunc, error) {
	var prefixes []string
	var actorTypes []string

	reqNamespace := add.GetNamespace()
	reqAppID := add.GetAppId()

	// A dead-letter stream serves no target, so it registers no deliverable
	// prefixes: doing so would stop undeliverable jobs from waiting for a
	// host, and release waiting jobs to the dead-letter stream instead.
	if dl := add.GetDeadLetter(); dl != nil {
		log.Infof("Dead-letter consumer connected: %s/%s (actorTypes=%v) (appIDs=%v).", reqNamespace, reqAppID, dl.GetActorTypes(), dl.GetAppIds())
		return func(error) {}, nil
	}

	ts := add.GetAcceptJobTypes()
	if len(ts) == 0 || slices.Contains(ts, schedulerv1pb.JobTargetType_JOB_TARGET_TYPE_JOB) {
		prefixes = append(prefixes, "app||"+reqNamespace+"||"+reqAppID+"||")
	}

	if len(ts) == 0 || slices.Contains(ts, schedulerv1pb.JobTargetType_JOB_TARGET_TYPE_ACTOR_REMINDER) {
		for _, actorType := range add.GetActorTypes() {
			prefixes = append(prefixes, "actorreminder||"+reqNamespace+"||"+actorType+"||")
		}

		actorTypes = add.GetActorTypes()
	}

	cancel, err := cron.DeliverablePrefixes(ctx, prefixes...)
	if err != nil {
		return nil, err
	}

	log.Infof("Sidecar connected: %s/%s (actorTypes=%v) (prefixes=%v).", reqNamespace, reqAppID, actorTypes, prefixes)

	return cancel, nil
}
