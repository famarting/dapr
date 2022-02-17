package pubsub

import (
	"strconv"

	"github.com/pkg/errors"
)

const (
	// BinaryCloudEventKey defines the metadata key for sending cloudevents in binary format in pubsub subscribers.
	BinaryCloudEventKey = "binaryCloudEvent"
)

type Subscription struct {
	PubsubName string            `json:"pubsubname"`
	Topic      string            `json:"topic"`
	Metadata   map[string]string `json:"metadata"`
	Rules      []*Rule           `json:"rules,omitempty"`
	Scopes     []string          `json:"scopes"`
}

type Rule struct {
	Match Expr   `json:"match"`
	Path  string `json:"path"`
}

type Expr interface {
	Eval(variables map[string]interface{}) (interface{}, error)
}

// IsBinaryCloudEvent determines if cloudevent should be sent to user's application in binary format (data in body, cloudevent attributes in headers).
func IsBinaryCloudEvent(metadata map[string]string) (bool, error) {
	if val, ok := metadata[BinaryCloudEventKey]; ok && val != "" {
		boolVal, err := strconv.ParseBool(val)
		if err != nil {
			return false, errors.Wrapf(err, "%s value must be a valid boolean: actual is '%s'", BinaryCloudEventKey, val)
		}

		return boolVal, nil
	}
	return false, nil
}
