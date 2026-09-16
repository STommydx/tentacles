# Production readiness and acceptance

The September 2026 hardening addresses the production review's code findings.
Passing automated checks is a prerequisite for rollout; a real GitHub canary
and soak are still required for the host's workload and credentials.

## Supported deployment

Use systemd **254 or newer** (Debian 13 or Ubuntu 24.04 are suitable examples),
a root supervisor, and a dedicated unprivileged runner user. Startup preflights
the version because credential delivery uses `--expand-environment=no`.
The shipped service creates state, payload cache, logs, and private runtime
directories, including after reboot. Pre-create the runner's shared caches
(`runner.shared_cache_paths` plus the `RUNNER_TOOL_CACHE` directory from
`runner.env`) and keep the service writable paths covering them when HOME or
the list changes.

Keep configuration, App key, state/cache parent directories, template, logs,
and JIT source directory controlled by root. Give only fresh slot descendants
to the runner. Do not recursively chown state during an upgrade: active slot
trees must preserve their runner ownership. The supervisor and job units are
independent so a supervisor restart preserves running jobs.

This remains a shared trusted host: all jobs use one runner UID and shared
caches. JIT values remain in runner argv for the process lifetime. Unlinking
credentials does not provide secure storage erasure.

## Review findings and fixes

| Finding | Resulting behavior |
|---|---|
| Missing runner identity and environment | App passes both; startup validates identity; systemd refuses root jobs |
| Unreadable JIT / unwritable payload | Fresh slot ownership transferred safely; systemd delivers private credentials |
| Malformed writable paths / sandbox cache failure | Whitespace-delimited quoted paths; supervisor-managed writable directories |
| Runtime directory missing after reboot | Private `RuntimeDirectory` recreated by systemd |
| Claim overwritten during startup | Mutex-protected state; claim survives successful or uncertain launch reply |
| Observation error erases a live job | Only confirmed exit permits cleanup; uncertain launches are retained; pre-launch failures carry explicit proof |
| Failed adoption erases surviving directories | Startup fails closed; allocation requires exclusive mkdir |
| Credential links damage outside files | Descriptor-relative unlink; no truncation or target mutation |
| Acquisition retry storm | Report failure immediately; check capped backoff before every launch |
| Admission oversubscription / memory double-count | Reserve starting and idle runners; subtract resident memory from predicted growth |
| Missing admission metric | Every hold increments the exported counter |
| Cleanup hangs / early ID reuse | Complete-operation deadlines; retain ID until worker actually finishes |
| Diagnostic collisions, unsafe files, unbounded disk | Unique private archives; no links/special files; copy limits and retention |
| Unbounded cancellation paths | Context-aware payload work, start/stop/adoption, HTTP and session shutdown |
| Old release pins | Go 1.26.8 and runner 2.337.0 with verified linux-x64 SHA256 |

Diagnostics default to seven days and 1 GiB of completed archives. Each new
archive is limited to 64 MiB, 4,096 entries, and 64 directory levels. Copying
preserves at least 256 MiB or 10% free disk, whichever is larger. Retention
runs before and after shipping and is serialized with it. Legacy unmarked
archives are intentionally preserved: inventory and remove those manually
according to your retention requirements during migration. Crashed staging
directories are also preserved for inspection and should be removed while
the supervisor is stopped; normal errors/cancellation remove their staging.

## Automated checks

```sh
go mod tidy -diff
gofmt -l .
go vet ./...
go test -race ./...
go install ./cmd/tentacles
for script in scripts/*.sh testdata/fake-runner/*.sh; do sh -n "$script" || exit; done
jq empty grafana/*.json
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
go run ./cmd/tentacles --config configs/config.example.yaml --dry-run
```

The regression suite covers job-claim interleavings, uncertain starts/exits,
pre-existing directories, cancellation, cleanup retry races, bounded retries,
admission reservations, hostile diagnostic entries, retention, and private
credential ownership. Linux CI additionally runs distinct-UID tests as root.

Run the isolated systemd smoke test on a disposable host:

```sh
go test -c -o /tmp/tentacles-systemd.test ./internal/systemd
sudo env \
  TENTACLES_SYSTEMD_SMOKE=1 \
  TENTACLES_SYSTEMD_SERVICE_UNIT="$PWD/configs/systemd/tentacles.service" \
  /tmp/tentacles-systemd.test \
  -test.run '^TestRealSystemdSmoke$' -test.v -test.timeout=90s
```

The smoke test starts a supervisor harness under the execution identity and
hardening directives read from the shipped service unit. The harness creates a
unique numeric transient slot unit and an isolated
`/var/lib/tentacles-smoke-*` directory. It verifies the supervisor can switch
to the actual runner UID, then checks credential source denial, credential
delivery, writable slot and shared cache, read-only template, discovery from a
fresh backend, and confirmed stop. It uses a dummy credential, calls no GitHub
API, and removes its units and directory after confirming exit. If exit cannot
be confirmed, it reports the retained path instead of deleting potentially
live files. The executable fixture is staged under `/var/lib` because `/run`
or `/tmp` may be mounted `noexec` or hidden by `PrivateTmp=`.

## Validation recorded on 2026-09-10

- Full `go test -race ./...`: passed, including all permanent regressions.
- Build, vet, formatting, diff whitespace, shell syntax and example dry-run: passed.
- `govulncheck` with Go 1.26.8: no vulnerabilities found.
- Root process test with a distinct UID: passed.
- Real systemd 257 smoke: passed; temporary test unit and directory removed.
- `systemd-analyze verify`: passed on temporary service copies with only the
  daemon executable path adjusted to the freshly built binary.
- Independent review: no remaining actionable safety findings after fixes.

The real GitHub canary, full service cold boot with App credentials, and
24-hour soak below have not been executed in this session.

## GitHub canary and soak

1. On a disposable host, install the service and root-owned configuration
   using a test App, restricted runner group, and disposable repository.
   Start from an empty cache/runtime tree and verify `READY=1`, payload
   download/checksum, directory owners, and an idle warm-pool runner.
2. Run a canary that checks UID, HOME, PATH/mise, working-directory writes,
   shared caches, a `setup-*` tool landing inside `RUNNER_TOOL_CACHE`, and
   denial of template/App-key/JIT-source reads or writes.
   Confirm exactly one job per runner and private diagnostics after exit.
3. Queue a burst beyond capacity. Check hard cap, resource reservations,
   admission-hold counter, and eventual service of queued work.
4. Restart the supervisor during a long job. Confirm the same job continues,
   the slot is adopted as busy, and no duplicate runner is registered.
5. Interrupt GitHub connectivity/JIT issuance and systemd command observation
   separately. Confirm bounded retries, responsive shutdown, and preservation
   of live files. Restore access and verify reconciliation recovers.
6. Exercise job cancellation, runner process death, low state/log disk space,
   malformed diagnostic entries, cleanup timeout, and a host reboot. Confirm
   no premature slot-ID reuse, external file damage, or permanent capacity leak.
7. Soak for at least 24 hours with starts/exits/reconnects. Compare active units,
   slot directories, registered runners, processes, memory, and disk before
   and after. Retain metrics and journals as acceptance evidence.

## Release maintenance

The runner 2.337.0 linux-x64 checksum is
`70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613`,
verified from the [official release API](https://api.github.com/repos/actions/runner/releases/tags/v2.337.0).
Go 1.26.8 keeps the project on its existing minor line with current patch
fixes; see [Go release downloads](https://go.dev/dl/).

Schedule runner upgrades, including checksum verification and a canary.
GitHub's [minimum-version enforcement timeline](https://github.blog/changelog/2026-06-12-github-actions-minimum-version-enforcement-timeline-for-self-hosted-runners/)
makes indefinitely pinned old runners unsuitable for production. Confirm the
version available to your organization during staged release rollout and
review GHES-specific requirements separately.
