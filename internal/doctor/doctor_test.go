package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		name    string
		results []Result
		want    int
	}{
		{"empty", nil, 0},
		{"all ok", []Result{{Status: OK}}, 0},
		{"warn is advisory", []Result{{Status: OK}, {Status: Warn}}, 0},
		{"a fail", []Result{{Status: OK}, {Status: Fail}}, 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.results); got != c.want {
			t.Errorf("%s: ExitCode = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRunExecutesChecksInOrder(t *testing.T) {
	results := Run([]Check{
		func() Result { return Result{Name: "a", Status: OK} },
		func() Result { return Result{Name: "b", Status: Warn} },
	})
	if len(results) != 2 || results[0].Name != "a" || results[1].Name != "b" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestReportShowsEachCheck(t *testing.T) {
	var b bytes.Buffer
	Report(&b, []Result{
		{Name: "container runtime", Status: OK, Detail: "docker found"},
		{Name: "cli", Status: Warn, Detail: "heads up"},
	})
	s := b.String()
	for _, want := range []string{"container runtime", "docker found", "cli", "heads up"} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q\n%s", want, s)
		}
	}
}

func TestContainerRuntimeFound(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name == "docker" {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}
	r := ContainerRuntime(lookPath)()
	if r.Status != OK {
		t.Errorf("status = %v, want OK", r.Status)
	}
	if !strings.Contains(r.Detail, "docker") {
		t.Errorf("detail should mention docker: %q", r.Detail)
	}
}

func TestContainerRuntimeMissing(t *testing.T) {
	lookPath := func(string) (string, error) { return "", errors.New("not found") }
	if r := ContainerRuntime(lookPath)(); r.Status != Warn {
		t.Errorf("status = %v, want Warn", r.Status)
	}
}
