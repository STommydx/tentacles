package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"os/user"
	"strconv"

	"github.com/0xinterface/tentacles/internal/runner"
)

// sampleSpec is the canonical slot configuration used by the golden
// Start-argument test.
func sampleSpec() runner.Spec {
	return runner.Spec{
		SlotDir:   "/var/lib/tentacles/pools/default/slots/0001",
		JITPath:   "/run/tentacles/default/0001.jit",
		EnvFile:   "/etc/tentacles/runner.env",
		User:      "gha-runner",
		Group:     "gha-runner",
		CPUQuota:  "400%",
		MemoryMax: "8G",
		UnitName:  "tentacle-default-0001.service",
	}
}

// writeFakeBin writes an executable sh script into dir and returns its
// path. Fake binaries must work on darwin, so they are plain /bin/sh.
func writeFakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const fakeSystemdRun = `
for arg in "$@"; do
	printf '%s\n' "$arg" >> "$FAKE_SYSTEMD_RUN_LOG"
done
if [ -n "$FAKE_SYSTEMD_RUN_STDERR" ]; then
	printf '%s\n' "$FAKE_SYSTEMD_RUN_STDERR" >&2
fi
exit "${FAKE_SYSTEMD_RUN_EXIT:-0}"
`

const fakeSystemctl = `
for arg in "$@"; do
	printf '%s\n' "$arg" >> "$FAKE_SYSTEMCTL_LOG"
done
case "$1" in
 show)
  case "$*" in
   *LoadState*)
    if [ -n "$FAKE_SYSTEMCTL_COUNT_FILE" ]; then
     n=0
     [ -f "$FAKE_SYSTEMCTL_COUNT_FILE" ] && n=$(cat "$FAKE_SYSTEMCTL_COUNT_FILE")
     n=$((n + 1)); printf '%s\n' "$n" > "$FAKE_SYSTEMCTL_COUNT_FILE"
     if [ "$n" -lt "${FAKE_SYSTEMCTL_FLIP_AT:-2}" ]; then
      printf 'LoadState=loaded\nActiveState=active\n'; exit 0
     fi
    fi
    printf 'LoadState=loaded\nActiveState=%s\n' "${FAKE_SYSTEMCTL_STATE:-inactive}"
    exit "${FAKE_SYSTEMCTL_EXIT:-0}";;
  esac
  printf 'CPUUsageNSec=%s\n' "${FAKE_SYSTEMCTL_CPU_NSEC:-0}"
		printf 'MemoryPeak=%s\n' "${FAKE_SYSTEMCTL_MEM_PEAK:-0}"
  printf 'MemoryCurrent=%s\n' "${FAKE_SYSTEMCTL_MEM_CURRENT:-0}"
		exit "${FAKE_SYSTEMCTL_EXIT:-0}"
		;;
	wait)
		if [ "${FAKE_SYSTEMCTL_WAIT_FAIL:-0}" = "1" ]; then
			printf 'Unknown operation wait.\n' >&2
			exit 1
		fi
		exit 0
		;;
	is-active)
		if [ -n "$FAKE_SYSTEMCTL_COUNT_FILE" ]; then
			n=0
			[ -f "$FAKE_SYSTEMCTL_COUNT_FILE" ] && n=$(cat "$FAKE_SYSTEMCTL_COUNT_FILE")
			n=$((n + 1))
			printf '%s\n' "$n" > "$FAKE_SYSTEMCTL_COUNT_FILE"
			if [ "$n" -lt "${FAKE_SYSTEMCTL_FLIP_AT:-2}" ]; then
				printf 'active\n'
				exit 0
			fi
		fi
		printf '%s\n' "${FAKE_SYSTEMCTL_STATE:-inactive}"
		exit 0
		;;
	list-units)
		if [ "${FAKE_SYSTEMCTL_LIST_FAIL:-0}" = "1" ]; then
			[ -n "$FAKE_SYSTEMCTL_STDERR" ] && printf '%s\n' "$FAKE_SYSTEMCTL_STDERR" >&2
			exit 1
		fi
		printf '%s\n' "$FAKE_SYSTEMCTL_LIST"
		exit 0
		;;
esac
if [ -n "$FAKE_SYSTEMCTL_STDERR" ]; then
	printf '%s\n' "$FAKE_SYSTEMCTL_STDERR" >&2
fi
exit "${FAKE_SYSTEMCTL_EXIT:-0}"
`

// newTestBackend wires a Backend to fake binaries that log their argv.
// Extra Options override the fake-bin defaults field by field.
func newTestBackend(t *testing.T, opts ...Options) (*Backend, string, string) {
	t.Helper()
	binDir := t.TempDir()
	runLog := filepath.Join(t.TempDir(), "systemd-run.log")
	ctlLog := filepath.Join(t.TempDir(), "systemctl.log")
	t.Setenv("FAKE_SYSTEMD_RUN_LOG", runLog)
	t.Setenv("FAKE_SYSTEMCTL_LOG", ctlLog)
	runBin := writeFakeBin(t, binDir, "systemd-run", fakeSystemdRun)
	ctlBin := writeFakeBin(t, binDir, "systemctl", fakeSystemctl)
	base := Options{
		SystemdRunBin: runBin,
		SystemctlBin:  ctlBin,
		StopTimeout:   30 * time.Second,
	}
	for _, o := range opts {
		if o.SystemdRunBin != "" {
			base.SystemdRunBin = o.SystemdRunBin
		}
		if o.SystemctlBin != "" {
			base.SystemctlBin = o.SystemctlBin
		}
		if o.StopTimeout != 0 {
			base.StopTimeout = o.StopTimeout
		}
		if o.CacheSubdirs != nil {
			base.CacheSubdirs = o.CacheSubdirs
		}
		if o.ExtraAddressFamilies != nil {
			base.ExtraAddressFamilies = o.ExtraAddressFamilies
		}
		if o.JobHome != "" {
			base.JobHome = o.JobHome
		}
		if o.Log != nil {
			base.Log = o.Log
		}
		if o.Namespace != "" {
			base.Namespace = o.Namespace
		}
	}
	b := New(base)
	return b, runLog, ctlLog
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStartGoldenArgVector(t *testing.T) {
	b, _, _ := newTestBackend(t)
	spec := sampleSpec()
	want := []string{
		"--collect",
		"--expand-environment=no",
		"--unit", "tentacle-default-0001.service",
		"--description", "GitHub Actions runner slot",
		"-p", "Type=exec",
		"-p", "User=gha-runner",
		"-p", "Group=gha-runner",
		"-p", "WorkingDirectory=/var/lib/tentacles/pools/default/slots/0001",
		"-p", "EnvironmentFile=/etc/tentacles/runner.env",
		"-p", "LoadCredential=jit:/run/tentacles/default/0001.jit",
		"-p", "CPUQuota=400%",
		"-p", "MemoryMax=8G",
		"-p", "Nice=5",
		"-p", "KillMode=mixed",
		"-p", "TimeoutStopSec=30",
		"-p", "TasksMax=4096",
		"-p", "PrivateTmp=yes",
		"-p", "NoNewPrivileges=yes",
		"-p", "CPUAccounting=yes",
		"-p", "MemoryAccounting=yes",
		"-p", "ProtectSystem=strict",
		"-p", "ProtectHome=read-only",
		"-p", `ReadWritePaths="/var/lib/tentacles/pools/default/slots/0001" "/tmp"`,
		"-p", "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		"-p", "LockPersonality=yes",
		"/bin/sh", "-c", `jit=$(cat "$CREDENTIALS_DIRECTORY/jit") || exit; exec ./run.sh --jitconfig "$jit"`,
	}

	if got := strings.Join(b.startArgs(spec), "\n") + "\n"; got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("Start argv mismatch\n--- got ---\n%s\n--- want ---\n%s", got, strings.Join(want, "\n"))
	}
}

// TestStartJobHomeArgVector: a configured job home adds the mount point to
// ReadWritePaths, binds the slot's own home over it, and hands the path to
// the launch script, which sets HOME after systemd applied EnvironmentFile.
// TestStartGoldenArgVector proves the vector is unchanged without it.
func TestStartJobHomeArgVector(t *testing.T) {
	b, _, _ := newTestBackend(t, Options{JobHome: "/var/lib/tentacles job-home"})
	args := b.startArgs(sampleSpec())
	got := strings.Join(args, "\n")
	for _, want := range []string{
		`ReadWritePaths="/var/lib/tentacles/pools/default/slots/0001" "/tmp" "/var/lib/tentacles job-home"`,
		`BindPaths="/var/lib/tentacles/pools/default/slots/0001/home":"/var/lib/tentacles job-home"`,
	} {
		if !strings.Contains(got, "-p\n"+want+"\n") {
			t.Errorf("property %q missing from args:\n%s", want, got)
		}
	}
	wantTail := []string{
		"/bin/sh", "-c",
		`HOME=$1; export HOME; jit=$(cat "$CREDENTIALS_DIRECTORY/jit") || exit; exec ./run.sh --jitconfig "$jit"`,
		"sh", "/var/lib/tentacles job-home",
	}
	if len(args) < len(wantTail) || !slices.Equal(args[len(args)-len(wantTail):], wantTail) {
		t.Errorf("command tail = %q, want %q", args[max(0, len(args)-len(wantTail)):], wantTail)
	}
}

// testStartSpec provides a fresh slot and a private JIT file using the
// current user's identity and an isolated HOME for cache creation.
func testStartSpec(t *testing.T) runner.Spec {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if account.Uid == "0" {
		t.Skip("this recording test uses current nonroot identity; distinct-UID test covers root")
	}
	spec := sampleSpec()
	spec.User = account.Username
	spec.Group = strconv.Itoa(os.Getegid())
	spec.SlotDir = t.TempDir()
	spec.JITPath = filepath.Join(t.TempDir(), "source.jit")
	if err := runner.WriteJIT(spec.JITPath, "secret"); err != nil {
		t.Fatal(err)
	}
	spec.EnvFile = filepath.Join(t.TempDir(), "runner.env")
	if err := os.WriteFile(spec.EnvFile, []byte("HOME="+t.TempDir()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestStartAddsRunnerCacheDirs(t *testing.T) {
	b, runLog, _ := newTestBackend(t)
	spec := testStartSpec(t)
	home := t.TempDir()
	if err := os.WriteFile(spec.EnvFile, []byte("HOME="+home+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	log := readLog(t, runLog)
	for _, sub := range runnerCacheSubdirs {
		path := filepath.Join(home, sub)
		if !strings.Contains(log, strconv.Quote(path)) {
			t.Errorf("cache path %q missing from args", path)
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			t.Errorf("cache dir %s not created: %v", sub, err)
		}
	}
	source, err := os.Stat(spec.JITPath)
	if err != nil {
		t.Fatal(err)
	}
	if source.Mode().Perm() != 0600 {
		t.Fatal("JIT source mode changed")
	}
}

// TestStartCreatesJobHome: the bind-mount source is created inside the fresh
// slot before it is handed to the runner, so it is removed with the slot.
func TestStartCreatesJobHome(t *testing.T) {
	b, runLog, _ := newTestBackend(t, Options{JobHome: t.TempDir()})
	spec := testStartSpec(t)
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(spec.SlotDir, jobHomeSubdir))
	if err != nil || !fi.IsDir() {
		t.Fatalf("job home not created as a directory: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("job home mode = %v, want 0700", fi.Mode().Perm())
	}
	if !strings.Contains(readLog(t, runLog), "BindPaths=") {
		t.Error("BindPaths missing from systemd-run args")
	}
}

// TestStartRejectsExistingJobHome: an entry already at <slot>/home means the
// slot is not fresh. Start must fail before systemd-run, provably unstarted,
// rather than bind whatever that entry points at.
func TestStartRejectsExistingJobHome(t *testing.T) {
	b, runLog, _ := newTestBackend(t, Options{JobHome: t.TempDir()})
	spec := testStartSpec(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(spec.SlotDir, jobHomeSubdir)); err != nil {
		t.Fatal(err)
	}
	err := b.Start(context.Background(), spec)
	if err == nil || !errors.Is(err, runner.ErrNotStarted) {
		t.Fatalf("Start() = %v, want an error wrapping ErrNotStarted", err)
	}
	if _, statErr := os.Stat(runLog); statErr == nil {
		t.Error("systemd-run was invoked despite the existing job home")
	}
}

func TestStartUsesConfiguredCacheSubdirs(t *testing.T) {
	b, runLog, _ := newTestBackend(t, Options{CacheSubdirs: []string{".npm", ".gradle"}})
	spec := testStartSpec(t)
	home := t.TempDir()
	if err := os.WriteFile(spec.EnvFile, []byte("HOME="+home+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	log := readLog(t, runLog)
	for _, sub := range []string{".npm", ".gradle"} {
		path := filepath.Join(home, sub)
		if !strings.Contains(log, strconv.Quote(path)) {
			t.Errorf("cache path %q missing from args", path)
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			t.Errorf("cache dir %s not created: %v", sub, err)
		}
	}
	for _, sub := range runnerCacheSubdirs {
		if strings.Contains(log, strconv.Quote(filepath.Join(home, sub))) {
			t.Errorf("default cache path %q leaked into args", sub)
		}
	}
}

func TestStartRestrictAddressFamilies(t *testing.T) {
	const prop = "RestrictAddressFamilies="
	find := func(b *Backend) string {
		for _, a := range b.startArgs(sampleSpec()) {
			if strings.HasPrefix(a, prop) {
				return strings.TrimPrefix(a, prop)
			}
		}
		t.Fatal("RestrictAddressFamilies property missing from args")
		return ""
	}

	t.Run("default is the base three", func(t *testing.T) {
		b, _, _ := newTestBackend(t)
		if got := find(b); got != "AF_UNIX AF_INET AF_INET6" {
			t.Errorf("families = %q, want the base three", got)
		}
	})

	t.Run("extras append after the base", func(t *testing.T) {
		b, _, _ := newTestBackend(t, Options{ExtraAddressFamilies: []string{"AF_NETLINK", "AF_PACKET"}})
		if got := find(b); got != "AF_UNIX AF_INET AF_INET6 AF_NETLINK AF_PACKET" {
			t.Errorf("families = %q, want the base three then the extras", got)
		}
	})

	t.Run("an extra repeating a base family is dropped", func(t *testing.T) {
		b, _, _ := newTestBackend(t, Options{ExtraAddressFamilies: []string{"AF_INET", "AF_NETLINK"}})
		if got := find(b); got != "AF_UNIX AF_INET AF_INET6 AF_NETLINK" {
			t.Errorf("families = %q, want no repeated base family", got)
		}
	})
}

// TestUsageReadsAccounting: the usage sampler reads cumulative CPU time
// and peak memory via systemctl show, feeding the per-workflow usage
// history.
func TestUsageReadsAccounting(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_CPU_NSEC", "25000000000") // 25s
	t.Setenv("FAKE_SYSTEMCTL_MEM_PEAK", "536870912")
	t.Setenv("FAKE_SYSTEMCTL_MEM_CURRENT", "268435456")
	u, err := b.Usage("tentacle-default-0001.service")
	if err != nil {
		t.Fatal(err)
	}
	if u.CurrentMemBytes != 268435456 {
		t.Fatalf("current mem = %v", u.CurrentMemBytes)
	}
	if u.CPUSeconds != 25 {
		t.Fatalf("cpu seconds = %v, want 25", u.CPUSeconds)
	}
	if u.PeakMemBytes != 536870912 {
		t.Fatalf("peak mem = %v", u.PeakMemBytes)
	}
}

// TestUsageErrorPropagates: a failed show surfaces as an error so the
// sampler keeps the previous reading instead of recording zeros.
func TestUsageErrorPropagates(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_STDERR", "failed to show unit")
	if _, err := b.Usage("tentacle-default-0001.service"); err == nil {
		t.Fatal("expected error from failed systemctl show")
	}
}

func TestStartRequiresRunnerIdentity(t *testing.T) {
	b, _, _ := newTestBackend(t)
	for _, name := range []string{"", "root", "tentacles-user-does-not-exist"} {
		spec := sampleSpec()
		spec.User = name
		if err := b.Start(context.Background(), spec); err == nil {
			t.Errorf("accepted runner user %q", name)
		}
	}
}

func TestStartErrorIncludesStderrTail(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMD_RUN_EXIT", "1")
	t.Setenv("FAKE_SYSTEMD_RUN_STDERR", "Failed to start transient service unit: Operation refused")

	err := b.Start(context.Background(), testStartSpec(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "tentacle-default-0001.service") {
		t.Errorf("error missing unit name: %v", err)
	}
	if !strings.Contains(err.Error(), "Operation refused") {
		t.Errorf("error missing stderr tail: %v", err)
	}
}

func TestStopArgvAndError(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)

	if err := b.Stop(context.Background(), "tentacle-default-0001.service"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if want := "stop\ntentacle-default-0001.service\n"; readLog(t, ctlLog) != want {
		t.Fatalf("Stop argv = %q, want %q", readLog(t, ctlLog), want)
	}

	t.Setenv("FAKE_SYSTEMCTL_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_STDERR", "Failed to stop unit: Connection timed out")
	err := b.Stop(context.Background(), "tentacle-default-0001.service")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Connection timed out") {
		t.Errorf("error missing stderr: %v", err)
	}
}

func TestWaitHappyPath(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)

	if err := b.Wait(context.Background(), "tentacle-default-0001.service"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if want := "show\ntentacle-default-0001.service\n--property=LoadState\n--property=ActiveState\n"; readLog(t, ctlLog) != want {
		t.Fatalf("Wait argv = %q, want %q", readLog(t, ctlLog), want)
	}
}

func TestWaitUsesStateObservation(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "inactive")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx, "tentacle-default-0001.service"); err != nil {
		t.Fatalf("Wait fallback: %v", err)
	}

	log := readLog(t, ctlLog)
	want := "show\ntentacle-default-0001.service\n--property=LoadState\n--property=ActiveState\n"
	if log != want {
		t.Fatalf("Wait fallback argv = %q, want %q", log, want)
	}
}

func TestWaitPollsUntilGone(t *testing.T) {
	b, _, ctlLog := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	// First is-active poll reports active, the second flips to inactive:
	// deterministic regardless of poll timing.
	t.Setenv("FAKE_SYSTEMCTL_COUNT_FILE", filepath.Join(t.TempDir(), "count"))
	t.Setenv("FAKE_SYSTEMCTL_FLIP_AT", "2")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "inactive")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Wait(ctx, "tentacle-default-0001.service"); err != nil {
		t.Fatalf("Wait fallback: %v", err)
	}
	log := readLog(t, ctlLog)
	if got, want := strings.Count(log, "--property=ActiveState"), 2; got != want {
		t.Fatalf("expected %d is-active polls, got %d:\n%s", want, got, log)
	}
}

func TestWaitContextCancellation(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_WAIT_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STATE", "active") // never goes inactive

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := b.Wait(ctx, "tentacle-default-0001.service")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait fallback err = %v, want context.DeadlineExceeded", err)
	}
}

func TestActiveParsesLegendOutput(t *testing.T) {
	b, _, controlLog := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST",
		"tentacle-default-0001.service loaded active running GitHub Actions runner slot\n"+
			"tentacle-default-0002.service loaded active running GitHub Actions runner slot\n")

	got, err := b.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	want := []string{"tentacle-default-0001.service", "tentacle-default-0002.service"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Active = %v, want %v", got, want)
	}
	if args := readLog(t, controlLog); !strings.Contains(args, "tentacle-default-*.service\n") {
		t.Fatalf("Active did not scope discovery to its namespace:\n%s", args)
	}
}

func TestActiveSkipsOverlappingPoolNamespace(t *testing.T) {
	backend, _, _ := newTestBackend(t)
	backend.namespace = "org-a"
	t.Setenv(
		"FAKE_SYSTEMCTL_LIST",
		"tentacle-org-a-0001.service loaded active running org-a runner\n"+
			"tentacle-org-a-b-0001.service loaded active running org-a-b runner\n",
	)

	got, err := backend.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	want := []string{"tentacle-org-a-0001.service"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Active = %v, want %v", got, want)
	}
}

func TestActiveEmptyOutput(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST", "")

	got, err := b.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Active = %v, want empty", got)
	}
}

func TestActiveReturnsCommandError(t *testing.T) {
	b, _, _ := newTestBackend(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST_FAIL", "1")
	t.Setenv("FAKE_SYSTEMCTL_STDERR", "Failed to create bus connection: No such file or directory")

	_, err := b.Active(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "No such file") {
		t.Errorf("error missing stderr: %v", err)
	}
}
