package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/target"
)

func TestExactProcessListenersRequiresStableIdentity(t *testing.T) {
	requested := Listener{PID: 42, Process: "node", CommandLine: "node app.js", Target: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}}
	listeners := []Listener{
		requested,
		{PID: 42, Process: "node", CommandLine: "node other.js", Target: requested.Target},
		{PID: 42, Process: "python", CommandLine: requested.CommandLine, Target: requested.Target},
		{PID: 99, Process: "node", CommandLine: requested.CommandLine, Target: requested.Target},
	}
	matches := exactProcessListeners(listeners, requested)
	if len(matches) != 1 || matches[0].PID != requested.PID || matches[0].CommandLine != requested.CommandLine {
		t.Fatalf("unstable process identity matched: %#v", matches)
	}
}

type fakeListenerObserver struct {
	snapshot ListenerSnapshot
	err      error
	calls    int
}

func (f *fakeListenerObserver) List(context.Context) (ListenerSnapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

func TestProcessTerminatorUsesListenerObserverSeam(t *testing.T) {
	observer := &fakeListenerObserver{snapshot: ListenerSnapshot{Authoritative: false}}
	terminator := &OSProcessTerminator{Observer: observer, OS: "darwin"}
	err := terminator.Terminate(context.Background(), Listener{PID: 4242, Process: "api", ProcessStart: "darwin:test", Target: target.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}})
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnknown {
		t.Fatalf("non-authoritative observation was not rejected: %v", err)
	}
	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want 1", observer.calls)
	}
}

func TestProcessTerminatorHonorsCancellationBeforeObservation(t *testing.T) {
	observer := &fakeListenerObserver{}
	terminator := &OSProcessTerminator{Observer: observer, OS: "darwin"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := terminator.Terminate(ctx, Listener{PID: 4242, Process: "api", ProcessStart: "darwin:test"})
	if err == nil || fault.AsAppError(err).Code != fault.ErrCancelled {
		t.Fatalf("cancelled termination was not rejected: %v", err)
	}
	if observer.calls != 0 {
		t.Fatalf("observer was called after cancellation: %d", observer.calls)
	}
}

func TestProcessTerminationProtectsCurrentProcess(t *testing.T) {
	d := &OSDiscoverer{OS: "darwin", Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		t.Fatal("protected process check should run before discovery")
		return runner.Result{}, nil
	})}
	err := d.Terminate(context.Background(), Listener{PID: 1, Process: "launchd", Target: target.Target{Address: "127.0.0.1", Port: 1, Protocol: "tcp"}})
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnsafe {
		t.Fatalf("protected process was not refused: %v", err)
	}
}

func TestParseLsofNormalizesAndPreservesPartialMetadata(t *testing.T) {
	listeners, err := ParseLsof("p0\nc\nn127.0.0.1:3000\np12\ncnode\nn[::]:8080\n", time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 2 {
		t.Fatalf("got %d listeners", len(listeners))
	}
	if listeners[0].Target.Port != 3000 || listeners[0].Target.Address != "127.0.0.1" {
		t.Fatalf("unexpected first target: %#v", listeners[0].Target)
	}
	if listeners[0].Metadata != MetadataPartial {
		t.Fatalf("metadata = %s, want partial", listeners[0].Metadata)
	}
	if listeners[1].Target.Address != "::" || listeners[1].Scope != target.ScopeWildcard {
		t.Fatalf("unexpected IPv6 target: %#v", listeners[1])
	}
}

func TestParseLsofRejectsMalformedEndpoint(t *testing.T) {
	if _, err := ParseLsof("p12\ncapp\nnot-an-endpoint\n", time.Now()); err == nil {
		t.Fatal("expected malformed lsof endpoint error")
	}
}

func TestParseLsofPreservesRowsBeforeMalformedRecord(t *testing.T) {
	partial, err := ParseLsof("p1\ncgood\nn127.0.0.1:3000\np2\ncbad\nnnot-an-endpoint\n", time.Unix(10, 0))
	if err == nil || len(partial) != 1 || partial[0].Process != "good" {
		t.Fatalf("partial lsof rows were not preserved: rows=%#v err=%v", partial, err)
	}
}

func TestParseSSExtractsProcessAndRejectsMalformedRows(t *testing.T) {
	text := `tcp LISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:(("api",pid=42,fd=3))`
	listeners, err := ParseSS(text, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 1 || listeners[0].PID != 42 || listeners[0].Process != "api" {
		t.Fatalf("unexpected parsed listener: %#v", listeners)
	}
	currentFormat := `LISTEN 0 128 127.0.0.1:8080 0.0.0.0:* users:(("api",pid=42,fd=3))`
	if listeners, err := ParseSS(currentFormat, time.Unix(10, 0)); err != nil || len(listeners) != 1 || listeners[0].PID != 42 {
		t.Fatalf("current ss format was not parsed: listeners=%#v err=%v", listeners, err)
	}
	partial, err := ParseSS(text+"\nmalformed", time.Unix(10, 0))
	if err == nil || len(partial) != 1 {
		t.Fatalf("partial ss rows were not preserved: rows=%#v err=%v", partial, err)
	}
	if _, err := ParseSS("tcp LISTEN 0 128 not-an-endpoint *", time.Now()); err == nil {
		t.Fatal("expected malformed ss endpoint error")
	}
}

func TestListUsesFakeCommandRunnerAndDoesNotClaimMissingSourceIsEmpty(t *testing.T) {
	d := &OSDiscoverer{
		OS:  "darwin",
		Now: func() time.Time { return time.Unix(20, 0) },
		Runner: runner.FuncRunner(func(ctx context.Context, name string, args ...string) (runner.Result, error) {
			if name != "lsof" {
				t.Fatal("unexpected command: " + name)
			}
			return runner.Result{Command: "lsof", Stdout: "p0\ncapp\nn127.0.0.1:3000\n", ExitCode: 0}, nil
		}),
	}
	snapshot, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Authoritative || len(snapshot.Listeners) != 1 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}

	d.Runner = runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{ExitCode: 1, Stderr: "permission denied"}, errors.New("permission denied")
	})
	snapshot, err = d.List(context.Background())
	if err == nil || snapshot.Authoritative || snapshot.Error == nil || snapshot.Error.Code != fault.ErrPermission {
		t.Fatalf("expected unavailable snapshot, got snapshot=%#v err=%v", snapshot, err)
	}
	if strings.Contains(snapshot.Error.Message, "token=") {
		t.Fatal("sensitive error was not redacted")
	}
}

func TestListPreservesPartialListenersWhenProviderReportsPermission(t *testing.T) {
	d := &OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p42\ncapi\nn127.0.0.1:8080\nP0\nTST=LISTEN\n", Stderr: "Permission denied for another process", ExitCode: 1}, errors.New("exit status 1")
	})}
	snapshot, err := d.List(context.Background())
	if err == nil || snapshot.Authoritative || len(snapshot.Listeners) != 1 || snapshot.Error == nil || snapshot.Error.Code != fault.ErrPermission {
		t.Fatalf("partial permission result was lost: snapshot=%#v err=%v", snapshot, err)
	}
}

func TestRedactCommandLineRemovesSensitiveValuesAndBoundsOutput(t *testing.T) {
	if got := redactCommandLine("app --token=secret --password hunter2 --name=ok"); strings.Contains(got, "secret") || strings.Contains(got, "hunter2") || !strings.Contains(got, "ok") {
		t.Fatalf("redaction failed: %q", got)
	}
	if got := redactCommandLine("curl -H Authorization:Bearer abc123 --api-key xyz789"); strings.Contains(got, "abc123") || strings.Contains(got, "xyz789") {
		t.Fatalf("header redaction failed: %q", got)
	}
	if got := redactCommandLine("curl https://example.test/callback?opaque=private"); strings.Contains(got, "private") {
		t.Fatalf("URL query redaction failed: %q", got)
	}
	if got := redactCommandLine("curl https://example.test/callback?opaque=private%ZZ"); strings.Contains(got, "private") {
		t.Fatalf("malformed URL query redaction failed: %q", got)
	}
	if got := redactCommandLine("app\x1b[31m"); strings.ContainsAny(got, "\x1b\n\r") {
		t.Fatalf("control characters were not sanitized: %q", got)
	}
	if got := redactCommandLine("curl https://user:private@example.test/callback#fragment"); strings.Contains(got, "user") || strings.Contains(got, "private") || strings.Contains(got, "fragment") {
		t.Fatalf("URL userinfo/fragment redaction failed: %q", got)
	}
	if got := redactCommandLine("curl HTTPS://user:private@example.test/callback#fragment"); strings.Contains(got, "user") || strings.Contains(got, "private") || strings.Contains(got, "fragment") {
		t.Fatalf("uppercase URL userinfo/fragment redaction failed: %q", got)
	}
	if got := redactCommandLine(strings.Repeat("x", 17000)); len(got) > 16*1024+32 {
		t.Fatalf("metadata was not bounded: %d", len(got))
	}
}

func TestClassifyCommandErrorPreservesTimeoutOverDiagnostics(t *testing.T) {
	err := classifyCommandError("discovery", "lsof", runner.Result{ExitCode: -1, Stderr: "permission denied", Truncated: true}, context.DeadlineExceeded)
	if err.Code != fault.ErrTimeout || err.Exit != fault.ErrTimeout.ExitCode() {
		t.Fatalf("timeout was misclassified: %#v", err)
	}
}

func TestParseSSProcessHandlesMissingPID(t *testing.T) {
	pid, process := parseSSProcess(`users:(("api",fd=3))`)
	if pid != 0 || process != "" {
		t.Fatalf("got pid=%d process=%q", pid, process)
	}
}
