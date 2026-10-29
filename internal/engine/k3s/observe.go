package k3s

import (
	"context"
	"errors"
	"fmt"

	"github.com/daropotter/chmura/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
)

// ErrObservationLost means continuity of runtime observation is unknown. It is
// distinct from a known instance exit/replacement and from probe health failures.
var ErrObservationLost = errors.New("runtime instance observation was lost")

// Observe establishes a watch from the selected generation's snapshot version,
// then calls onStart once with that snapshot's target. onStart must return
// promptly; the owner can start checks there and must cancel/wait for them when
// Observe returns. Changes to Kubernetes readiness do not end observation.
// The watch is stopped on return. Observation does not pin an IP, restart work,
// reconnect after a gap, or adopt another generation; it reports loss of
// continuity so the owner can reconcile explicitly.
func (r Resolver) Observe(ctx context.Context, ref Instance, ports map[string]manifest.Port, onStart func(Target)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if onStart == nil {
		return errors.New("instance observation requires a start observer")
	}
	pod, err := r.readPod(ctx, ref)
	if err != nil {
		return err
	}
	target, err := targetFromPod(pod, ref, ports)
	if err != nil {
		return err
	}
	if pod.ResourceVersion == "" {
		return observationFailure(ctx, errors.New("snapshot has no resource version"))
	}
	stream, err := r.Client.Pods(ref.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:       fields.OneTermEqualSelector("metadata.name", ref.PodName).String(),
		ResourceVersion:     pod.ResourceVersion,
		AllowWatchBookmarks: true,
	})
	if err != nil {
		return observationFailure(ctx, err)
	}
	defer stream.Stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	onStart(target)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, open := <-stream.ResultChan():
			if !open {
				return observationFailure(ctx, errors.New("watch stream closed"))
			}
			switch event.Type {
			case watch.Bookmark:
				continue
			case watch.Error:
				return observationFailure(ctx, apierrors.FromObject(event.Object))
			case watch.Added, watch.Modified, watch.Deleted:
				pod, ok := event.Object.(*corev1.Pod)
				if !ok || pod == nil || pod.Name == "" || pod.Namespace == "" || pod.UID == "" || pod.ResourceVersion == "" {
					return observationFailure(ctx, errors.New("watch returned an incomplete pod identity"))
				}
				current, err := targetFromPod(pod, ref, nil)
				if err != nil {
					return err
				}
				if event.Type == watch.Deleted {
					return fmt.Errorf("%w: pod was deleted", ErrInstanceUnavailable)
				}
				if current.Network.Host != target.Network.Host || !current.ProcessStartedAt.Equal(target.ProcessStartedAt) {
					return observationFailure(ctx, errors.New("instance target changed"))
				}
			default:
				return observationFailure(ctx, fmt.Errorf("unexpected watch event %q", event.Type))
			}
		}
	}
}

func observationFailure(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	return fmt.Errorf("%w: %w", ErrObservationLost, err)
}
