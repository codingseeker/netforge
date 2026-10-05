package recovery

import (
	"context"
	"strconv"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/probe"
)

// Criteria is what has to be measured for a recovery to count as verified.
//
// The default is strict on purpose. A path that still drops packets after the
// action is not recovered, and reporting it as recovered is the one failure
// mode worse than reporting nothing.
type Criteria struct {
	// MaxLoss is the largest loss a verified path may show, as a percentage.
	MaxLoss float64
	// MaxRTT is the slowest average round trip a verified path may show.
	MaxRTT time.Duration
	// MinReplies is how many answers have to come back at all. Zero loss with
	// no replies is not a healthy path, it is a probe that never ran.
	MinReplies int
}

// DefaultCriteria returns criteria that accept a clean path and nothing else.
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

// judge measures a path against the criteria. It returns the reason as well as
// the verdict, because a verification that fails without saying what it saw is
// a verification nobody can act on.
// Judge decides whether a measurement meets the criteria, and says so in a
// sentence either way. It is exported because a verdict is needed in two places:
// after a recovery action, and at the end of an experiment that performed none.
// An experiment that measured a broken path and called it verified would be
// reporting the one thing its measurements contradict.
func (c Criteria) Judge(measurement probe.Measurement) (bool, string) {
	if measurement.Received < c.MinReplies {
		return false, "only " + strconv.Itoa(measurement.Received) + " of " + strconv.Itoa(measurement.Sent) +
			" probes were answered, at least " + strconv.Itoa(c.MinReplies) + " are required"
	}
	// The loss is derived from the counts rather than read from the reported
	// field. The counts are what the kernel said happened, and a measurement
	// whose own field disagrees with them is not a measurement a repair should
	// be certified against.
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

// Verification is the measurement that backs a recovery.
type Verification struct {
	// Namespace is the namespace the probe ran in.
	Namespace string
	// Target is the address that was probed.
	Target string
	// Measurement is what the path reported.
	Measurement probe.Measurement
	// Passed reports whether the measurement met the criteria.
	Passed bool
	// Reason is the verdict in a sentence, either way.
	Reason string
	// At is when the measurement was taken.
	At time.Time
}

// Prober measures a path. It is the one way a recovery learns whether its work
// mattered: the criteria above are applied to a real measurement, never to the
// absence of an error from the action.
type Prober interface {
	Measure(ctx context.Context, namespace, target string, options probe.Options) (probe.Measurement, error)
}
