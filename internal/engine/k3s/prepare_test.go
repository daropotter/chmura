package k3s_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/engine/k3s"
	"github.com/daropotter/chmura/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

func creationRequest(t *testing.T) k3s.InstanceRequest {
	t.Helper()
	project, err := manifest.Parse([]byte(`version: 1
name: Shop Name
applications:
  Public API:
    source: {image: registry.example.com/team/api:v1}
    ports:
      web listener: {number: 8080, protocol: http}
      alias: {number: 8080, protocol: tcp}
      secure: {number: 8443, protocol: https}
      dns: {number: 5353, protocol: udp}
    health:
      check: {http: {path: /ready, port: web listener}}
      healthy:
        unhealthy-failures: 3
        check: {tcp: {port: web listener}}
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := deployment.PlanInstance(project, "Public API")
	if err != nil {
		t.Fatal(err)
	}
	return k3s.InstanceRequest{
		Namespace: "project-a", IdempotencyKey: "request-1", Plan: *plan,
		ResolvedImage: "registry.example.com/team/api@sha256:" + strings.Repeat("a", 64),
		NodeName:      "node-a", CPUMilli: 250, MemoryBytes: 128 * 1024 * 1024,
	}
}

func TestPrepareInstanceUsesPinnedImageAndExplicitAllocation(t *testing.T) {
	request := creationRequest(t)
	pod, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Namespace != request.Namespace || pod.Name == "" || pod.GenerateName != "" || pod.Spec.NodeName != request.NodeName || len(pod.Spec.Containers) != 1 {
		t.Fatalf("instance identity/placement = %#v", pod)
	}
	c := pod.Spec.Containers[0]
	if c.Name != "app" || c.Image != request.ResolvedImage || c.ImagePullPolicy != corev1.PullIfNotPresent || len(c.Command) != 0 || len(c.Args) != 0 {
		t.Fatalf("image execution = %#v", c)
	}
	for _, resources := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
		cpu, memory := resources[corev1.ResourceCPU], resources[corev1.ResourceMemory]
		if cpu.MilliValue() != request.CPUMilli || memory.Value() != request.MemoryBytes || len(resources) != 2 {
			t.Fatalf("allocation = %#v", resources)
		}
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Spec.ReadinessGates) != 1 || pod.Spec.ReadinessGates[0].ConditionType != k3s.ReadinessGate || len(pod.Status.Conditions) != 0 {
		t.Fatal("instance can bypass its generation's Chmura readiness/lifecycle")
	}
	if c.ReadinessProbe != nil || c.LivenessProbe != nil || c.StartupProbe != nil {
		t.Fatal("Chmura health rules were replaced by native probes")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks || pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Fatal("instance acquired ambient credentials, service variables or host namespaces")
	}
	if len(c.Ports) != 3 {
		t.Fatalf("native listener aliases were not deduplicated: %#v", c.Ports)
	}
	for _, port := range c.Ports {
		if port.Name != "" || port.HostPort != 0 || port.HostIP != "" || port.Protocol != corev1.ProtocolTCP && port.Protocol != corev1.ProtocolUDP {
			t.Fatalf("portable listener became an engine name/host binding: %#v", port)
		}
	}
}

func TestPrepareInstanceBindsKeyAndIntentDeterministically(t *testing.T) {
	request := creationRequest(t)
	request.IdempotencyKey = strings.Repeat("opaque / key ", 100)
	first, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || len(validation.IsDNS1123Subdomain(first.Name)) != 0 || len(first.Name) > 63 {
		t.Fatal("request replay did not produce the same valid runtime payload")
	}
	request.Plan.Health.Ready.Successes++
	changed, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Name != first.Name || reflect.DeepEqual(changed.Annotations, first.Annotations) {
		t.Fatal("key reuse with different health intent cannot be detected")
	}
	request.IdempotencyKey = "another-request"
	other, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	if other.Name == first.Name {
		t.Fatal("different request keys selected the same runtime name")
	}
}

func TestPrepareInstanceDoesNotMutateOrAliasItsInputs(t *testing.T) {
	request := creationRequest(t)
	first, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := k3s.PrepareInstance(request)
	if err != nil {
		t.Fatal(err)
	}
	first.Annotations["changed"] = "yes"
	*first.Spec.EnableServiceLinks = true
	if *first.Spec.AutomountServiceAccountToken {
		t.Fatal("changing service links also enabled service-account token mounting")
	}
	first.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
	first.Spec.Containers[0].Ports[0].ContainerPort = 9999
	if !strings.HasSuffix(request.Plan.Image, ":v1") || request.Plan.Health.Check.HTTP.Path != "/ready" || request.Plan.Ports["web listener"].Number != 8080 {
		t.Fatal("payload preparation modified the portable plan")
	}
	cpu := second.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if second.Annotations["changed"] != "" || cpu.MilliValue() != 250 || second.Spec.Containers[0].Ports[0].ContainerPort == 9999 {
		t.Fatal("separate prepared payloads share mutable data")
	}
}

func TestPrepareInstanceCanonicalizesResolvedImageOnly(t *testing.T) {
	request := creationRequest(t)
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, source := range []string{"api:v1", "docker.io/library/api@" + digest} {
		request.Plan.Image = source
		request.ResolvedImage = "docker.io/library/api:ignored@" + digest
		pod, err := k3s.PrepareInstance(request)
		if err != nil {
			t.Fatal(err)
		}
		if pod.Spec.Containers[0].Image != "docker.io/library/api@"+digest || request.Plan.Image != source {
			t.Fatal("resolved image was not canonical or the declaration was changed")
		}
	}
}

func TestPrepareInstanceRejectsIncompleteOrUnsupportedRequests(t *testing.T) {
	for _, mode := range []string{"namespace", "invalid namespace", "key", "node", "invalid node", "cpu", "memory", "image tag", "different repository", "pinned source changed", "project", "application", "process-only readiness", "exec readiness", "exec healthy", "negative timeout", "zero healthy threshold", "port number", "port protocol"} {
		t.Run(mode, func(t *testing.T) {
			request := creationRequest(t)
			switch mode {
			case "namespace":
				request.Namespace = ""
			case "invalid namespace":
				request.Namespace = "bad_namespace"
			case "key":
				request.IdempotencyKey = " "
			case "node":
				request.NodeName = ""
			case "invalid node":
				request.NodeName = "bad/node"
			case "cpu":
				request.CPUMilli = 0
			case "memory":
				request.MemoryBytes = -1
			case "image tag":
				request.ResolvedImage = request.Plan.Image
			case "different repository":
				request.ResolvedImage = "other.example.com/api@sha256:" + strings.Repeat("a", 64)
			case "pinned source changed":
				request.Plan.Image = "registry.example.com/team/api@sha256:" + strings.Repeat("b", 64)
			case "project":
				request.Plan.ProjectName = ""
			case "application":
				request.Plan.ApplicationName = ""
			case "process-only readiness":
				request.Plan.Readiness, request.Plan.Health.Check = deployment.ReadinessProcessOnly, nil
			case "exec readiness":
				request.Plan.Health.Check = &manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"/bin/check"}}}
			case "exec healthy":
				request.Plan.Health.Healthy.Check = &manifest.Probe{Exec: &manifest.ExecCheck{Command: manifest.Command{"/bin/check"}}}
			case "negative timeout":
				request.Plan.Health.Check.Timeout = -1
			case "zero healthy threshold":
				request.Plan.Health.Healthy.UnhealthyFailures = 0
			case "port number":
				request.Plan.Ports["bad"] = manifest.Port{Number: 70000, Protocol: "tcp"}
			case "port protocol":
				request.Plan.Ports["bad"] = manifest.Port{Number: 9000, Protocol: "invalid"}
			}
			if pod, err := k3s.PrepareInstance(request); pod != nil || err == nil {
				t.Fatalf("invalid/unsupported request produced a payload: pod=%#v error=%v", pod, err)
			}
		})
	}
}
