# AegiSheet

[![Go](https://github.com/Redpatronus/AegiSheet/actions/workflows/go.yml/badge.svg)](https://github.com/Redpatronus/AegiSheet/actions/workflows/go.yml)
[![FOSSA Status](https://app.fossa.io/api/projects/git%2Bgithub.com%2Foccu-io%2Fgsreport.svg?type=shield)](https://app.fossa.io/projects/git%2Bgithub.com%2Foccu-io%2Fgsreport?ref=badge_shield)

Google Workspace administrative reporting. AegiSheet reads the Admin SDK
(Directory and Reports) and writes an Excel workbook with one worksheet per
report — the artefact an access review, a 2FA rollout or an audit response
actually needs.

Available reports:

| Task | Contents |
| --- | --- |
| `users-without-2fa` | Accounts that can still authenticate with a password alone |
| `suspended-users` | Suspended accounts and the reason for suspension |
| `admins` | Super and delegated administrators, de-duplicated, with roles |
| `groups` | Groups with membership counts and aliases |
| `group-members` | Direct members of every group, with role and status |
| `app-specific-passwords` | Long-lived credentials that bypass 2-step verification |
| `third-party-tokens` | OAuth grants to external applications, with full scope lists |
| `audit-activities` | Admin SDK audit log for one application, over a time window |

Run `aegisheet -list-tasks` for the authoritative list, including the aliases
accepted for backwards compatibility.

## Install

Requires Go 1.25 or newer.

```sh
make build          # produces ./bin/aegisheet with version metadata
# or
go install github.com/Redpatronus/AegiSheet/cmd/aegisheet@latest
```

## Quick start

```sh
cp conf/xls_report.ini conf/mine.ini    # then edit the [global] section
aegisheet -config conf/mine.ini -output report.xlsx
```

The configuration file describes **what** to report on. Anything under
`[global]` can also come from the environment or the command line:

```sh
AEGISHEET_OUTPUT=/srv/reports/workspace.xlsx \
  aegisheet -config conf/mine.ini -log-format json -only Admins,Groups
```

Useful before a real run:

```sh
aegisheet -config conf/mine.ini -print-config   # how the layers resolved
aegisheet -config conf/mine.ini -dry-run        # run every report, write nothing
```

## Configuration

Four layers, each overriding the one before it:

1. built-in defaults
2. the `[global]` section of the INI file
3. environment variables, `AEGISHEET_*`
4. command line flags

Report sections come only from the INI file, since they describe the work rather
than the environment; `-only` and `-skip` select subsets of them.

`aegisheet -h` documents every flag with its default. Each one has an
environment and INI spelling too — `-log-level`, `AEGISHEET_LOG_LEVEL`,
`logLevel` — and `-print-config` prints all three columns side by side with the
resolved value.

INI keys are case-insensitive, so `userKey`, `userkey` and `USERKEY` are the
same key.

### Report sections

Each section becomes one worksheet; the section name is the worksheet title,
normalised to Excel's rules (31 characters, no `: \ / ? * [ ]`).

```ini
[Admin Activity]
task=GetAuditLogins     ; or audit-activities
appname=drive           ; login, admin, drive, token, mobile, groups, ...
lookbackDays=30         ; or startTime=2026-01-01 / endTime=2026-02-01
;userKey=all            ; "all" (default) or one address
;eventName=login_failure
;maxRows=50000          ; safety valve for a busy tenant
;sheet=Drive Audit      ; override the worksheet title
;enabled=false          ; keep the section, skip the run
```

A section with no `task=` key is skipped with a warning; an unrecognised task
name is an error that lists the valid ones.

## Authentication

Pick one of three modes.

**Service account with domain-wide delegation** — the right choice for
scheduled runs. No browser, no terminal, no cached token.

```sh
aegisheet -service-account /etc/aegisheet/sa.json \
          -impersonate workspace-admin@example.com
```

Grant the delegation in the Admin console under *Security → Access and data
control → API controls → Domain-wide delegation*, using the scopes AegiSheet
requests (below).

**Installed application OAuth** — prompts once on the terminal, then reuses the
cached token, refreshing and re-persisting it as needed.

```sh
aegisheet -credentials credentials.json -token token.json
```

**Application Default Credentials** — for use inside Google Cloud:
`aegisheet -use-adc`.

Set `-non-interactive` for cron and CI so that a missing or expired token fails
immediately instead of blocking forever on a read from standard input.

### Scopes

AegiSheet requests read-only scopes wherever the API offers them:

- `admin.directory.user.readonly`
- `admin.directory.group.readonly`
- `admin.directory.user.security` — required to list application specific
  passwords and third-party tokens; no read-only variant exists
- `admin.reports.audit.readonly`

Deployments that do not run those two security reports can narrow the set
further with `-scopes`. Changing scopes invalidates a cached token: delete the
token file so the next run re-authorises.

## Handling the data

The workbook contains directory data about real people. Treat it accordingly:

- it is written **atomically** — a crash cannot leave a truncated file where a
  valid previous report was — and with mode `0600` by default (`-output-mode`);
- **logs are PII-safe by default.** Addresses are masked (`a***@example.com`),
  identity is stripped from logged request paths, and credentials are redacted
  unconditionally. `-log-pii` disables masking for short debugging sessions and
  makes the log as sensitive as the report itself;
- credential files that are readable beyond their owner produce a warning with
  the `chmod` needed to fix them.

## Operations

Logging is structured (`log/slog`): `-log-format json` for a collector,
`-log-file` to append to a file, `-log-level debug` to see every HTTP request.

Exit codes are stable and intended for schedulers:

| Code | Meaning |
| --- | --- |
| 0 | every configured section succeeded |
| 1 | the run failed |
| 2 | the configuration or command line was rejected |
| 3 | authentication failed |
| 4 | the report was produced, but at least one section failed |
| 130 | interrupted by a signal |

By default one failing section does not abandon the others: the failure is
logged, the remaining reports still run, the workbook is still written, and the
exit code is 4 so a scheduler notices. `-fail-fast` stops at the first error
instead. An interrupted or timed-out run writes nothing, so a partial report
never replaces a complete one.

Transient failures — 429, 403 rate limits, 5xx, dropped connections — are
retried with jittered exponential backoff (`-max-retries`,
`-retry-base-delay`, `-retry-max-delay`) and honour a `Retry-After` header.
Reports that make one call per user or per group issue them with bounded
parallelism (`-concurrency`, default 4).

Example cron entry:

```cron
0 6 * * 1 /usr/local/bin/aegisheet \
    -config /etc/aegisheet/report.ini \
    -service-account /etc/aegisheet/sa.json \
    -impersonate workspace-admin@example.com \
    -output /srv/reports/workspace-$(date +\%Y-\%m-\%d).xlsx \
    -log-format json -log-file /var/log/aegisheet.log -non-interactive
```

## Layout

```
cmd/aegisheet        entry point: wiring, signals, exit codes
internal/config      defaults, INI, environment, flags, validation
internal/logging     structured logging with PII and secret redaction
internal/gsuite      authentication, retry policy, paginated Admin SDK access
internal/tasks       the report implementations
internal/xlsx        worksheet construction and atomic workbook writing
internal/runner      orchestration, per-section error isolation, run summary
internal/buildinfo   build identity
```

## Development

```sh
make check      # gofmt, go vet, tests with the race detector
make test
make build
```

## Upgrading from `gsreport`

Existing configuration files keep working: the legacy task names
(`GetUsersWithout2Fa`, `GetAuditLogins`, …) are accepted as aliases, section
names still become worksheet titles, and `[users] whitelist` is still honoured
(with a warning pointing at `[global]`). Behaviour differs in ways worth
knowing about:

- **Reports are complete.** List calls follow pagination; previously anything
  past the first page was silently missing.
- **A missing `task=` key no longer defaults to the 2FA report.** The section is
  skipped with a warning, and an unknown task name is rejected outright.
- **`admins` de-duplicates.** Super administrators are no longer listed twice.
- **`audit-activities` no longer overwrites rows.** Events without parameters
  produce a row, and every row carries its full context.
- **`app-specific-passwords` writes a worksheet** instead of only logging.
- **`third-party-tokens` sweeps every user by default** rather than requiring a
  `userKey`.
- **Group members without an address are included**, not skipped.
- **Read-only scopes.** The tool no longer requests directory *write* access.
- The default configuration path is `conf/xls_report.ini`, which is the file
  the repository actually ships.

## License

[![FOSSA Status](https://app.fossa.io/api/projects/git%2Bgithub.com%2Foccu-io%2Fgsreport.svg?type=large)](https://app.fossa.io/projects/git%2Bgithub.com%2Foccu-io%2Fgsreport?ref=badge_large)
