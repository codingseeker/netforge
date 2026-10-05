package recovery

import (
	"context"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

type Criteria struct {
	MaxLoss float64

	MaxRTT time.Duration

	MinReplies int
}

func DefaultCriteria() Criteria {
	return Criteria{MaxLoss: 0, MaxRTT: 2 * time.Second, MinReplies: 1}
}

func (c Criteria) check(op string, limits Limits) (Criteria, error) {
	switch {
	case c.MaxLoss < 0 || c.MaxLoss > 100:
		return c, apperr.InvalidArgument(op, "criteria.max_loss is "+strconv.FormatFloat(c.MaxLoss, 'g', -1, 64)+
			", which is not a percentage", nil)
	case c.MaxRTT <= 0:
		return c, apperr.InvalidArgument(op, "criteria.max_rtt must be positive", nil)
	case c.MaxRTT > limits.MaxDeadline:
		return c, apperr.LimitExceeded(op, "criteria.max_rtt "+c.MaxRTT.String()+
			" is longer than limits.max_deadline "+limits.MaxDeadline.String(), nil)
	case c.MinReplies < 0:
		return c, apperr.InvalidArgument(op, "criteria.min_replies must not be negative", nil)
	}
	return c, nil
}

func (c Criteria) Judge(measurement probe.Measurement) (bool, string) {
	if measurement.Received < c.MinReplies {
		return false, "only " + strconv.Itoa(measurement.Received) + " of " + strconv.Itoa(measurement.Sent) +
			" probes were answered, at least " + strconv.Itoa(c.MinReplies) + " are required"
	}

	loss := measurement.Loss
	if measurement.Sent > 0 && measurement.Received < measurement.Sent {
		derived := 100 * float64(measurement.Sent-measurement.Received) / float64(measurement.Sent)
		if derived > loss {
			loss = derived
		}
	}
	if loss > c.MaxLoss {
		return false, "loss is " + strconv.FormatFloat(loss, 'g', -1, 64) + "%, at most " +
			strconv.FormatFloat(c.MaxLoss, 'g', -1, 64) + "% is allowed"
	}
	if measurement.AverageRTT > c.MaxRTT {
		return false, "average round trip is " + measurement.AverageRTT.String() + ", at most " +
			c.MaxRTT.String() + " is allowed"
	}
	return true, "loss is " + strconv.FormatFloat(loss, 'g', -1, 64) + "% over " +
		strconv.Itoa(measurement.Received) + " replies, average round trip " + measurement.AverageRTT.String()
}

type Verification struct {
	Namespace string

	Target string

	Measurement probe.Measurement

	Passed bool

	Reason string

	At time.Time
}

type Prober interface {
	Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error)
}
