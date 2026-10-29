package k3s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	ReadinessGate    corev1.PodConditionType = "chmura/ready"
	keyAnnotation                            = "chmura/request-key-sha256"
	intentAnnotation                         = "chmura/request-sha256"
	managedLabel                             = "app.kubernetes.io/managed-by"
	instanceLabel                            = "chmura/instance"
)

// InstanceRequest combines a validated portable plan with explicit engine facts.
// The owner must supply an existing, prepared project namespace and an admitted
// node/allocation. ResolvedImage is the declared image resolved to a named digest;
// resolving tags and verifying registry content belong to the owner.
type InstanceRequest struct {
	Namespace      string
	IdempotencyKey string
	Plan           deployment.InstancePlan
	ResolvedImage  string
	NodeName       string
	CPUMilli       int64
	MemoryBytes    int64
}

// ErrUnsupportedPlan means this executor cannot implement the requested checks.
var ErrUnsupportedPlan = errors.New("instance plan requires unsupported readiness or healthy checks")

// PrepareInstance builds one Pod without cluster I/O. Runtime names are derived
// from the namespace-scoped request key; full key/intent hashes allow the writer
// to reject collisions and key reuse. The initial executor supports HTTP/TCP
// readiness and optional HTTP/TCP or process-only healthy monitoring.
func PrepareInstance(request InstanceRequest) (*corev1.Pod, error) {
	if len(validation.IsDNS1123Label(request.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(request.NodeName)) != 0 || strings.TrimSpace(request.IdempotencyKey) == "" || request.CPUMilli <= 0 || request.MemoryBytes <= 0 {
		return nil, errors.New("instance creation requires a namespace, request key and admitted node/CPU/memory allocation")
	}
	plan := request.Plan
	if strings.TrimSpace(plan.ProjectName) == "" || strings.TrimSpace(plan.ApplicationName) == "" {
		return nil, errors.New("instance creation requires explicit project/application names")
	}
	image, err := resolvedImage(plan.Image, request.ResolvedImage)
	if err != nil {
		return nil, err
	}
	plan.Health = *plan.Health.Effective()
	if plan.Readiness != deployment.ReadinessChecked || !networkCheck(plan.Health.Check, plan.Ports) {
		return nil, ErrUnsupportedPlan
	}
	if plan.Health.StartupTimeout <= 0 || plan.Health.Ready.Successes <= 0 || plan.Health.Ready.UnreadyFailures <= 0 {
		return nil, errors.New("instance health timings and thresholds must be positive")
	}
	if healthy := plan.Health.Healthy; healthy != nil {
		if healthy.UnhealthyFailures <= 0 {
			return nil, errors.New("instance healthy threshold must be positive")
		}
		if healthy.Check != nil && !networkCheck(healthy.Check, plan.Ports) {
			return nil, ErrUnsupportedPlan
		}
	}
	ports := make([]corev1.ContainerPort, 0, len(plan.Ports))
	for _, port := range plan.Ports {
		protocol := corev1.ProtocolTCP
		switch port.Protocol {
		case "http", "https", "tcp":
		case "udp":
			protocol = corev1.ProtocolUDP
		default:
			return nil, errors.New("instance port protocol is invalid")
		}
		if port.Number < 1 || port.Number > 65535 {
			return nil, errors.New("instance port number is invalid")
		}
		ports = append(ports, corev1.ContainerPort{ContainerPort: int32(port.Number), Protocol: protocol})
	}
	slices.SortFunc(ports, func(a, b corev1.ContainerPort) int {
		if a.ContainerPort != b.ContainerPort {
			return int(a.ContainerPort - b.ContainerPort)
		}
		return strings.Compare(string(a.Protocol), string(b.Protocol))
	})
	ports = slices.Compact(ports)
	key := sha256.Sum256([]byte(request.IdempotencyKey))
	keyHash := hex.EncodeToString(key[:])
	name := "chmura-" + keyHash[:32]
	intent, err := json.Marshal(struct {
		Version     int
		Namespace   string
		Plan        deployment.InstancePlan
		Image       string
		Node        string
		CPU, Memory int64
	}{1, request.Namespace, plan, image, request.NodeName, request.CPUMilli, request.MemoryBytes})
	if err != nil {
		return nil, fmt.Errorf("encode instance intent: %w", err)
	}
	fingerprint := sha256.Sum256(intent)
	allocation := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(request.CPUMilli, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(request.MemoryBytes, resource.BinarySI),
	}
	noToken, noLinks, grace := false, false, int64(30)
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Namespace: request.Namespace, Name: name,
			Labels:      map[string]string{managedLabel: "chmura", instanceLabel: name},
			Annotations: map[string]string{keyAnnotation: keyHash, intentAnnotation: hex.EncodeToString(fingerprint[:])}},
		Spec: corev1.PodSpec{
			NodeName: request.NodeName, RestartPolicy: corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &noToken, EnableServiceLinks: &noLinks,
			DNSPolicy: corev1.DNSClusterFirst, SchedulerName: corev1.DefaultSchedulerName,
			TerminationGracePeriodSeconds: &grace,
			ReadinessGates:                []corev1.PodReadinessGate{{ConditionType: ReadinessGate}},
			Containers: []corev1.Container{{Name: "app", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
				Ports: ports, Resources: corev1.ResourceRequirements{Requests: allocation, Limits: allocation.DeepCopy()},
				TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile}},
		},
	}, nil
}

func resolvedImage(declared, resolved string) (string, error) {
	source, err := reference.ParseNormalizedNamed(declared)
	if err != nil {
		return "", fmt.Errorf("declared image: %w", err)
	}
	named, err := reference.ParseNormalizedNamed(resolved)
	if err != nil {
		return "", fmt.Errorf("resolved image: %w", err)
	}
	pinned, ok := named.(reference.Canonical)
	if !ok || !strings.EqualFold(reference.Domain(source), reference.Domain(named)) || reference.Path(source) != reference.Path(named) {
		return "", errors.New("resolved image must name the declared repository and a digest")
	}
	if original, ok := source.(reference.Canonical); ok && original.Digest() != pinned.Digest() {
		return "", errors.New("resolved image changed the declared digest")
	}
	base, err := reference.WithName(strings.ToLower(reference.Domain(named)) + "/" + reference.Path(named))
	if err != nil {
		return "", err
	}
	canonical, err := reference.WithDigest(base, pinned.Digest())
	if err != nil {
		return "", err
	}
	return canonical.String(), nil
}

func networkCheck(check *manifest.Probe, ports map[string]manifest.Port) bool {
	if check == nil || check.Process != nil || check.Exec != nil || (check.HTTP == nil) == (check.TCP == nil) || check.Interval <= 0 || check.Timeout <= 0 {
		return false
	}
	if check.HTTP != nil {
		p, exists := ports[check.HTTP.Port]
		u, err := url.ParseRequestURI(check.HTTP.Path)
		if !exists || p.Protocol != "http" && p.Protocol != "https" || err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || len(check.HTTP.ExpectStatus) == 0 {
			return false
		}
		for _, code := range check.HTTP.ExpectStatus {
			if code < 100 || code > 599 {
				return false
			}
		}
		return true
	}
	p, exists := ports[check.TCP.Port]
	return exists && (p.Protocol == "http" || p.Protocol == "https" || p.Protocol == "tcp")
}
