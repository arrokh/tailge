package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestParseProcessUsageConvertsCPUAndResidentMemory(t *testing.T) {
	usage, err := parseProcessUsage("  12.5  1024\n")
	if err != nil {
		t.Fatal(err)
	}
	if usage.CPUPercent != 12.5 || usage.MemoryBytes != 1024*1024 || usage.MemorySource != ProcessMemoryRSS {
		t.Fatalf("usage = %#v, want 12.5%% CPU and 1 MiB RSS", usage)
	}
}

func TestParseProcessUsageRejectsInvalidOrAmbiguousOutput(t *testing.T) {
	for _, output := range []string{"", "0.0", "NaN 1024", "-1.0 1024", "0.5 -1", "0.5 18446744073709551615", "1,5 1024", "0.1 1 extra"} {
		if usage, err := parseProcessUsage(output); err == nil {
			t.Errorf("parseProcessUsage(%q) = %#v, want an error", output, usage)
		}
	}
}

func TestProcessUsagesPrefersDarwinPhysicalFootprint(t *testing.T) {
	directory := t.TempDir()
	ps := filepath.Join(directory, "ps")
	psScript := "#!/bin/sh\nprintf '4242 12.5 1024\\n4343 0.0 2048\\n'\n"
	if err := os.WriteFile(ps, []byte(psScript), 0o755); err != nil {
		t.Fatal(err)
	}
	footprintCalls := filepath.Join(directory, "footprint-calls")
	footprint := filepath.Join(directory, "footprint")
	footprintScript := `#!/bin/sh
[ "$LC_ALL" = C ] || exit 9
printf '%s\n' "$4" >> "$TAILGE_FOOTPRINT_CALLS"
case "$4" in
4242) printf '%s\n' 'OrbStack Helper [4242]: 64-bit Footprint: 1 B' 'Auxiliary data:' '    phys_footprint: 13260138336 B' '    phys_footprint_peak: 19767413424 B' ;;
4343) printf '%s\n' 'api [4343]: 64-bit Footprint: 1 B' 'Auxiliary data:' '    phys_footprint: 2097152 B' '    phys_footprint_peak: 2097152 B' ;;
*) exit 10 ;;
esac
`
	if err := os.WriteFile(footprint, []byte(footprintScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("TAILGE_FOOTPRINT_CALLS", footprintCalls)

	usages := processUsages(context.Background(), "darwin", []int{4242, 4343})
	if usage := usages[4242]; usage == nil || usage.CPUPercent != 12.5 || usage.MemoryBytes != 13260138336 || usage.MemorySource != ProcessMemoryPhysicalFootprint {
		t.Fatalf("OrbStack usage = %#v, want 12.5%% CPU and 13260138336-byte physical footprint", usage)
	}
	if usage := usages[4343]; usage == nil || usage.MemoryBytes != 2097152 {
		t.Fatalf("second process usage = %#v, want 2097152-byte physical footprint", usage)
	}
	calls, err := os.ReadFile(footprintCalls)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, pid := range strings.Fields(string(calls)) {
		seen[pid] = true
	}
	if len(seen) != 2 || !seen["4242"] || !seen["4343"] {
		t.Fatalf("footprint calls = %q; want one independent lookup for each unique PID", calls)
	}
}

func TestParseProcessUsageRowAndBatchPsLookup(t *testing.T) {
	pid, usage, err := parseProcessUsageRow("  4242  12.5  1024")
	if err != nil || pid != 4242 || usage.CPUPercent != 12.5 || usage.MemoryBytes != 1024*1024 {
		t.Fatalf("parseProcessUsageRow = (%d, %#v, %v)", pid, usage, err)
	}

	directory := t.TempDir()
	callsFile := filepath.Join(directory, "calls")
	ps := filepath.Join(directory, "ps")
	script := "#!/bin/sh\n[ \"$LC_ALL\" = C ] || exit 9\nprintf 'call\\n' >> \"$TAILGE_PS_CALLS\"\nprintf '4242 12.5 1024\\n4343 0.0 2048\\n'\n"
	if err := os.WriteFile(ps, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("TAILGE_PS_CALLS", callsFile)
	usages := processUsages(context.Background(), "darwin", []int{4242, 4343})
	if len(usages) != 2 || usages[4242] == nil || usages[4343] == nil || usages[4343].MemoryBytes != 2*1024*1024 || usages[4242].MemorySource != ProcessMemoryRSS {
		calls, _ := os.ReadFile(callsFile)
		t.Fatalf("batched process usages = %#v; script calls=%q", usages, calls)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil || string(calls) != "call\n" {
		t.Fatalf("ps call count = %q, err=%v; want one call", calls, err)
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
