// Package k3s adapts Chmura instance operations to the Kubernetes API.
package k3s

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"time"

	"github.com/daropotter/chmura/internal/manifest"
	"github.com/daropotter/chmura/internal/probe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Instance identifies one container generation. These are private engine facts,
// never fields in a project manifest or the public application model.
type Instance struct {
	Namespace     string
	PodName       string
	PodUID        string
	ContainerName string
	ContainerID   string
}

// Target is a point-in-time observation of the selected process generation.
type Target struct {
	Network          probe.Target
	ProcessStartedAt time.Time
	ResourceVersion  string
}

// ErrStaleInstance means the observed pod or container replaced the selected generation.
var ErrStaleInstance = errors.New("runtime instance has been replaced")

// ErrInstanceUnavailable means the selected generation has no running probe target.
var ErrInstanceUnavailable = errors.New("runtime instance is unavailable")

// Resolver reads runtime facts through an explicitly configured client. It does
// not load local kubeconfig, select a different instance, or change the cluster.
type Resolver struct {
	Client coreclient.CoreV1Interface
}

// Resolve verifies the selected pod/container generation and returns its primary
// IP, independently copied declared ports, and actual container start time.
// Kubernetes readiness is not Chmura readiness. The caller supplies validated
// ports and owns the lookup deadline. Resolution does not pin an IP to a
// generation; lifecycle supervision must stop probes when that generation exits.
func (r Resolver) Resolve(ctx context.Context, ref Instance, ports map[string]manifest.Port) (Target, error) {
	pod, err := r.readPod(ctx, ref)
	if err != nil {
		return Target{}, err
	}
	return targetFromPod(pod, ref, ports)
}

func (r Resolver) readPod(ctx context.Context, ref Instance) (*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.Client == nil || ref.Namespace == "" || ref.PodName == "" || ref.PodUID == "" || ref.ContainerName == "" || ref.ContainerID == "" {
		return nil, errors.New("target resolution requires a client and a complete instance generation")
	}
	pod, err := r.Client.Pods(ref.Namespace).Get(ctx, ref.PodName, metav1.GetOptions{})
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime instance: %w", err)
	}
	return pod, nil
}

func targetFromPod(pod *corev1.Pod, ref Instance, ports map[string]manifest.Port) (Target, error) {
	if pod == nil {
		return Target{}, ErrInstanceUnavailable
	}
	if pod.Namespace != ref.Namespace || pod.Name != ref.PodName || string(pod.UID) != ref.PodUID {
		return Target{}, fmt.Errorf("%w: pod identity changed", ErrStaleInstance)
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return Target{}, fmt.Errorf("%w: pod is not running or is terminating", ErrInstanceUnavailable)
	}
	declared := false
	for _, container := range pod.Spec.Containers {
		if container.Name == ref.ContainerName {
			declared = true
			break
		}
	}
	if !declared {
		return Target{}, fmt.Errorf("%w: application container is absent", ErrInstanceUnavailable)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != ref.ContainerName {
			continue
		}
		if status.ContainerID != "" && status.ContainerID != ref.ContainerID {
			return Target{}, fmt.Errorf("%w: container identity changed", ErrStaleInstance)
		}
		if status.ContainerID == "" || status.State.Running == nil || status.State.Running.StartedAt.IsZero() {
			return Target{}, fmt.Errorf("%w: application container is not running", ErrInstanceUnavailable)
		}
		ip, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			return Target{}, fmt.Errorf("%w: pod has no usable primary IP", ErrInstanceUnavailable)
		}
		return Target{
			Network:          probe.Target{Host: ip.String(), Ports: maps.Clone(ports)},
			ProcessStartedAt: status.State.Running.StartedAt.Time,
			ResourceVersion:  pod.ResourceVersion,
		}, nil
	}
	return Target{}, fmt.Errorf("%w: application container status is absent", ErrInstanceUnavailable)
}
