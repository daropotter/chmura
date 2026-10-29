package deployment_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/daropotter/chmura/internal/deployment"
	"github.com/daropotter/chmura/internal/manifest"
)

func parseProject(t *testing.T, data string) *manifest.Project {
	t.Helper()
	project, err := manifest.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func imageProject(t *testing.T, image string) *manifest.Project {
	t.Helper()
	return parseProject(t, fmt.Sprintf("version: 1\nname: shop\napplications:\n  app:\n    source: {image: %q}\n", image))
}

const checkedProject = `version: 1
name: shop
applications:
  app:
    source: {image: registry.example.com/team/api:1.0}
    ports:
      web: {number: 8080, protocol: http}
    health:
      check: {http: {path: /ready, port: web}}
      healthy:
        unhealthy-failures: 3
        check: {exec: {command: [/bin/self-check, ""]}}
`

func TestPlanImageInstanceResolvesEffectiveSettings(t *testing.T) {
	project := parseProject(t, checkedProject)
	plan, err := deployment.PlanInstance(project, "app")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProjectName != "shop" || plan.ApplicationName != "app" || plan.Image != "registry.example.com/team/api:1.0" || plan.Readiness != deployment.ReadinessChecked {
		t.Fatalf("selected instance plan = %#v", plan)
	}
	if port := plan.Ports["web"]; port.Number != 8080 || port.Protocol != "http" || port.Visibility != "project" {
		t.Fatalf("planned port = %#v", port)
	}
	health := plan.Health
	if health.StartupTimeout != manifest.Duration(2*time.Minute) || health.Ready.Successes != 1 || health.Ready.UnreadyFailures != 3 {
		t.Fatalf("lifecycle defaults = %#v", health)
	}
	if health.Check.Interval != manifest.Duration(5*time.Second) || health.Check.Timeout != manifest.Duration(2*time.Second) || !reflect.DeepEqual(health.Check.HTTP.ExpectStatus, []int{200}) {
		t.Fatalf("readiness defaults = %#v", health.Check)
	}
	if health.Healthy.Check.Interval != manifest.Duration(5*time.Second) || health.Healthy.Check.Timeout != manifest.Duration(2*time.Second) || health.Healthy.UnhealthyFailures != 3 || health.Healthy.Check.HTTP != nil {
		t.Fatalf("independent healthy rule = %#v", health.Healthy)
	}
	declared := project.Applications["app"].Health
	if declared.StartupTimeout != 0 || declared.Check.Interval != 0 || declared.Check.Timeout != 0 || declared.Check.HTTP.ExpectStatus != nil {
		t.Fatal("planning filled omissions in the declared manifest")
	}
}

func TestPlanImageInstancePreservesNamesAndOverrides(t *testing.T) {
	project := parseProject(t, `version: 1
name: Shop Name
applications:
  Public API:
    source: {image: registry.example.com:5000/team/api:v2}
    ports:
      HTTPS listener: {number: 8443, protocol: https, visibility: space}
    health:
      check:
        http: {path: /ready, port: HTTPS listener, expect-status: [204]}
        interval: 3s
        timeout: 500ms
      ready: {successes: 2, unready-failures: 4}
      startup-timeout: 30s
      healthy:
        unhealthy-failures: 6
        check:
          process: {name: worker}
          interval: 7s
          timeout: 1s
`)
	plan, err := deployment.PlanInstance(project, "Public API")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProjectName != "Shop Name" || plan.ApplicationName != "Public API" || plan.Ports["HTTPS listener"].Visibility != "space" {
		t.Fatalf("logical names or visibility changed: %#v", plan)
	}
	if !reflect.DeepEqual(&plan.Health, project.Applications["Public API"].Health) {
		t.Fatal("planning changed explicitly declared health settings")
	}
}

func TestPlanImageInstanceCopiesMutableInputs(t *testing.T) {
	project := parseProject(t, checkedProject)
	first, err := deployment.PlanInstance(project, "app")
	if err != nil {
		t.Fatal(err)
	}
	second, err := deployment.PlanInstance(project, "app")
	if err != nil {
		t.Fatal(err)
	}
	first.Ports["web"] = manifest.Port{Number: 9999}
	first.Health.Check.HTTP.Path = "/changed"
	first.Health.Check.HTTP.ExpectStatus[0] = 500
	first.Health.Healthy.Check.Exec.Command[0] = "/changed"
	declared := project.Applications["app"]
	if declared.Ports["web"].Number != 8080 || declared.Health.Check.HTTP.Path != "/ready" || declared.Health.Healthy.Check.Exec.Command[0] != "/bin/self-check" {
		t.Fatal("plan mutations reached the declaration")
	}
	if second.Ports["web"].Number != 8080 || second.Health.Check.HTTP.Path != "/ready" || second.Health.Check.HTTP.ExpectStatus[0] != 200 || second.Health.Healthy.Check.Exec.Command[0] != "/bin/self-check" {
		t.Fatal("separate instance plans share mutable data")
	}
	declared.Source.Image = "other:v1"
	declared.Ports["web"] = manifest.Port{Number: 9000}
	declared.Health.Healthy.Check.Exec.Command[0] = "/source-changed"
	project.Applications["app"] = declared
	if second.Image != "registry.example.com/team/api:1.0" || second.Ports["web"].Number != 8080 || second.Health.Healthy.Check.Exec.Command[0] != "/bin/self-check" {
		t.Fatal("an existing plan changed with its source")
	}
}

func TestPlanImageInstanceMarksProcessOnlyWithoutAddingProbes(t *testing.T) {
	for _, tc := range []struct {
		name, health string
		healthy      bool
	}{
		{"omitted", "", false},
		{"empty", "    health: {}\n", false},
		{"ready rules only", "    health: {ready: {successes: 2}}\n", false},
		{"process-only healthy", "    health: {healthy: {unhealthy-failures: 2}}\n", true},
		{"own healthy check", "    health: {healthy: {unhealthy-failures: 2, check: {exec: {command: [/bin/self-check]}}}}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "version: 1\nname: shop\napplications:\n  app:\n    source: {image: worker:v1}\n" + tc.health
			project := parseProject(t, data)
			plan, err := deployment.PlanInstance(project, "app")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Readiness != deployment.ReadinessProcessOnly || plan.Health.Check != nil || (plan.Health.Healthy != nil) != tc.healthy {
				t.Fatalf("process-only mode enabled/inherited a check: %#v", plan)
			}
			if plan.Health.StartupTimeout != manifest.Duration(2*time.Minute) {
				t.Fatalf("process-only lifecycle defaults missing: %#v", plan.Health)
			}
			if tc.name == "omitted" && project.Applications["app"].Health != nil {
				t.Fatal("planning inserted health into the declaration")
			}
			if tc.name == "process-only healthy" && plan.Health.Healthy.Check != nil {
				t.Fatal("planning added a healthy probe")
			}
		})
	}
}

func TestPlanImageInstancePreservesValidImageReferences(t *testing.T) {
	for _, image := range []string{
		"busybox",
		"registry.example.com:5000/team/api:v1",
		"Registry.EXAMPLE.com/team/api:v1",
		"registry.example.com/team/api@sha256:" + strings.Repeat("a", 64),
		"registry.example.com/team/api:v1@sha256:" + strings.Repeat("b", 64),
		"registry.example.com/team/api@sha512:" + strings.Repeat("c", 128),
	} {
		t.Run(image, func(t *testing.T) {
			plan, err := deployment.PlanInstance(imageProject(t, image), "app")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Image != image {
				t.Fatalf("image was rewritten: %q, want %q", plan.Image, image)
			}
		})
	}
}

func TestPlanImageInstanceRejectsMalformedImageReferences(t *testing.T) {
	for _, image := range []string{"https://registry.example.com/api:v1", "image with spaces", "registry.example.com/UPPERCASE/repository", "app@sha256:bad", "app@sha256:" + strings.Repeat("a", 63), "app:" + strings.Repeat("a", 129)} {
		t.Run(image, func(t *testing.T) {
			plan, err := deployment.PlanInstance(imageProject(t, image), "app")
			if plan != nil || err == nil || !strings.Contains(err.Error(), "source.image") {
				t.Fatalf("invalid image was planned: plan=%#v error=%v", plan, err)
			}
		})
	}
}

func TestPlanImageInstanceRequiresExplicitSelection(t *testing.T) {
	for _, mode := range []string{"nil project", "schema", "project name", "empty application", "missing application"} {
		t.Run(mode, func(t *testing.T) {
			project := imageProject(t, "api:v1")
			name := "app"
			switch mode {
			case "nil project":
				project = nil
			case "schema":
				project.Version = 2
			case "project name":
				project.Name = " "
			case "empty application":
				name = ""
			case "missing application":
				name = "other"
			}
			if plan, err := deployment.PlanInstance(project, name); plan != nil || err == nil {
				t.Fatalf("implicit/invalid selection: plan=%#v error=%v", plan, err)
			}
		})
	}
}

func TestPlanImageInstanceRequiresPrebuiltSource(t *testing.T) {
	project := parseProject(t, `version: 1
name: shop
applications:
  built:
    source: {context: ., dockerfile: Dockerfile}
  image:
    source: {image: api:v1}
`)
	if plan, err := deployment.PlanInstance(project, "image"); err != nil || plan.Image != "api:v1" {
		t.Fatalf("unselected build source affected image plan: plan=%#v error=%v", plan, err)
	}
	if plan, err := deployment.PlanInstance(project, "built"); plan != nil || !errors.Is(err, deployment.ErrBuildRequired) {
		t.Fatalf("Dockerfile source was treated as prebuilt: plan=%#v error=%v", plan, err)
	}
	for _, source := range []manifest.Source{{}, {Image: " "}, {Image: "api:v1", Context: ".", Dockerfile: "Dockerfile"}} {
		project.Applications["image"] = manifest.Application{Source: source}
		if plan, err := deployment.PlanInstance(project, "image"); plan != nil || err == nil {
			t.Fatalf("invalid source was planned: plan=%#v error=%v", plan, err)
		}
	}
}

func TestPlanImageInstanceIsRepeatable(t *testing.T) {
	project := parseProject(t, checkedProject)
	first, err := deployment.PlanInstance(project, "app")
	if err != nil {
		t.Fatal(err)
	}
	second, err := deployment.PlanInstance(project, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("unchanged desired state produced different plans")
	}
}
