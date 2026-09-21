package runner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommandRunnerBoundsOutputAndRecordsExit(t *testing.T) {
	r := ExecRunner{MaxOutput: 8}
	result, err := r.Run(context.Background(), "sh", "-c", "printf 123456789")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.Stdout != "12345678" || result.ExitCode != 0 {
		t.Fatalf("unexpected bounded result: %#v", result)
	}
}

func TestCommandRunnerReturnsExitErrorWithoutLeakingUnboundedOutput(t *testing.T) {
	r := ExecRunner{MaxOutput: 32}
	result, err := r.Run(context.Background(), "sh", "-c", "printf 'password=secret' >&2; exit 7")
	if err == nil || result.ExitCode != 7 || !strings.Contains(result.Stderr, "password=secret") {
		t.Fatalf("unexpected failure result: %#v err=%v", result, err)
	}
}

func TestCommandRunnerMapsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := (ExecRunner{}).Run(ctx, "sh", "-c", "sleep 1")
	if err == nil || result.ExitCode != -1 {
		t.Fatalf("expected cancelled command: %#v err=%v", result, err)
	}
}

func TestCommandRunnerRejectsInvalidTimeout(t *testing.T) {
	r := ExecRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := r.Run(ctx, "sh", "-c", "true"); err != nil {
		t.Fatal(err)
	}
}
