package controller

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/go-logr/stdr"
)

func TestRunPreflightStepLogsCompletion(t *testing.T) {
	var buf bytes.Buffer
	logger := stdr.NewWithOptions(log.New(&buf, "", 0), stdr.Options{})

	if err := runPreflightStep(logger, "target vCenter", func() error { return nil }, "server", "vc.example.com"); err != nil {
		t.Fatalf("runPreflightStep: %v", err)
	}

	for _, want := range []string{"preflight check started", "preflight check completed", "target vCenter", "vc.example.com", "duration"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("preflight log missing %q; got:\n%s", want, buf.String())
		}
	}
}
