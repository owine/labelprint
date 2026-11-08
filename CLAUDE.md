# CLAUDE.md

Guidance for Claude Code in this repository. User-facing behaviour and configuration live in `README.md`; this file
covers how the code is built and the constraints that aren't visible from it.

**Versions are not restated here.** Go, the base images and every apk pin live in `go.mod` and the `Dockerfile`. The
current release is the latest published GitHub Release (`gh release view`).

## What it is

A single Go binary that serves an embedded page (`web/`) and talks IPP to LPrint, which runs
on the Docker host (not in a container) and drives the label printer; developed against a Zebra GX420t. It
runs behind a reverse proxy with an auth layer, since it has no login of its own. `deploy/compose.yaml` is an
example stack. The repo is public: keep real hostnames, addresses and deployment details out of it.

| file | role |
|---|---|
| `ipp.go` | hand-written IPP/2.0 encoder/decoder and the four operations used: Print-Job, Cancel-Job, Get-Printer-Attributes, Get-Jobs |
| `detect.go` | content sniffing (PDF/PNG/JPEG magic, ZPL by `.zpl` or `^XA…^XZ`), page-range parsing |
| `render.go` | `pdfinfo` + `pdftoppm` wrapper; one call per page, scratch files on the `/tmp` tmpfs |
| `zpl.go` | ZPL preview via zebrash (the only module dependency, `github.com/Gobbo89/zebrash`): panic recovery, timeout, canvas from `^PW`/`^LL` or the media |
| `media.go` | turns LPrint's `media-supported` PWG names into dropdown labels and sizes |
| `server.go` | HTTP handlers, upload limits, the print flow, cross-origin protection, security headers |
| `maintenance.go` | `POST /api/printer/maintenance`: "feed a label" as a ZPL job (a one-dot box). No remote calibrate: that's SmartCal at the printer |
| `share.go` | phone sharing: `POST /share` holds a checked upload in memory under a one-time token for the page's preview |
| `web/` | the page: vanilla JS, owine design tokens (`tokens.css`), vendored fonts. No build step |

## Constraints that shaped the code

Verified against the live LPrint (Get-Printer-Attributes) and the LPrint/PAPPL sources:

- LPrint accepts `application/vnd.zebra-zpl`, `image/png`, `image/jpeg`, `image/pwg-raster`, `image/urf`. **No PDF**,
  so PDFs are rasterised here.
- `multiple-document-jobs-supported = false`, so it's one Print-Job per PDF page.
- `page-ranges` isn't a job-creation attribute, so ranges are applied at render time.
- LPrint's ZPL driver (`lprint_zpl_printfile`) copies raw data through once and **ignores `copies`**. ZPL copies are
  made by repeating the document in a single job. Don't "fix" this by sending `copies`.
- PAPPL's image path does honour `copies`, `print-scaling` and auto-rotation, so PNG/JPEG/PDF pages rely on them.
- zebrash is the [Gobbo89 fork](https://github.com/Gobbo89/zebrash): upstream
  [ingridhq/zebrash](https://github.com/ingridhq/zebrash) has been unmaintained since its maintainer left Ingrid in
  2026, and the fork carries the fixes (Code 128 `>5`, size limits for malformed ZPL). It's one person's fork, so
  Renovate never automerges it; read each update's diff before merging.
- zebrash has no panic recovery of its own and is fed untrusted uploads, so `zpl.go` recovers and times out around
  it. The fork's malformed-ZPL limits don't change that: keep both. A failed ZPL preview is reported in the page
  (`previewError`) and must never block printing.
- The printer can report `media-empty` while printing fine, so state reasons are shown as warnings and never block a
  print.
- Cancel-Job is in `operations-supported` and takes `printer-uri` + `job-id`. PAPPL refuses a completed, canceled or
  aborted job with `client-error-not-possible`, and an unknown id with `client-error-not-found`; those messages are
  shown as LPrint sent them. A processing job only stops at its next page, so it can read "processing" for a moment
  after a successful cancel. Without an LPrint `auth-service`, PAPPL doesn't check that the canceller owns the job.
- A share (`/share`, the manifest's `share_target`) only stages a file for the preview and never prints. Shares stay
  in memory, single use, for at most 10 minutes. `web/sw.js` has no fetch handler on purpose. Without any service
  worker, Android Chrome loads the start page and waits up to 5 s before sending a share. A fetch handler would see
  every request, and must never cache anything. The app icons are `web/favicon.svg` rasterised with
  `rsvg-convert` (192 and 512 px). The maskable icon is the same mark without the rounded corners.

Never send to port 9100 directly. Every job must go through LPrint so it appears in LPrint's job list.

## Privacy

Labels contain names and addresses. Don't add any outside service (Labelary etc.) for rendering or previews;
ZPL previews use zebrash in-process for exactly this reason. Don't log
filenames or document contents (logs carry kind, size, copies, media and job ids only), and never commit real label
files (`.gitignore` blocks `*.zpl` outside `testdata/`). The page loads nothing third-party; the CSP is
`default-src 'self'`.

## Commands

```sh
go test -race ./...                                  # PDF tests skip without poppler
UPDATE_FIXTURES=1 go test -run Fixture               # after changing makeLabelPDF
docker buildx build --platform linux/arm64 -t labelprint:test --load .
docker run --rm -p 8080:8080 --read-only --cap-drop ALL --tmpfs /tmp labelprint:test
```

Lint like CI: `gofmt -l .`, `go vet ./...`, hadolint on `Dockerfile`, `yamllint .github/ deploy/`, actionlint.

## CI/CD

- `ci.yml`: lint and `go test`, then `docker-build` builds each arch on a native runner (`ubuntu-26.04` and
  `ubuntu-26.04-arm`) and smoke-tests it hardened: refuses to start without `LPRINT_URI`, read-only, no caps,
  non-root, PDF and ZPL render, font substitution, cross-site POST refused. The Trivy HIGH/CRITICAL gate runs before
  anything is pushed. On `main`, `push-image` pushes each arch by digest, `publish-image` joins them and tags
  `:sha-<long>` (and `:main` if still head), and `code-scanning` uploads Trivy's full SARIF report. Jobs that run PR
  code hold no write token. Keep the matrix `runner:` lines in their one-line shape; Renovate's `github-runners`
  manager tracks them.
- `release-drafter.yml` + `.github/release-drafter.yml`: the shared fleet config from `owine/renovate-config`. The
  embedded `release-summary.js` is an identical fleet copy; don't edit it here.
- `release.yml` (fleet copy from doc-scanner): publishing the draft retags the `:sha-<long>` digest as
  `X.Y.Z`/`X.Y`/`X`/`latest`, without a rebuild. Image tags have no `v`.

PR titles are conventional commits (`feat:`, `fix:`, `deps:` … see `owine/renovate-config`'s README). They drive the
release notes and version bump. PRs are squash-merged.

## Dependency management

Renovate extends the `owine/renovate-config` presets (base, automerge, go, docker, alpine). The apk pins track the
Alpine line set by `branch=v3.XX` in `.github/renovate.json`. When the Alpine base crosses a release line, bump the base
tag and digest, every apk pin, and that `branch=` in one PR.

The shared `go` preset owns the Go rules; the only Go rule in this repo is the zebrash one (below). The preset gives: `go mod tidy` on every gomod
update, indirect modules tracked (that's where zebrash's CVEs land), `golang.org/x/*` as one group, and one
`go toolchain` PR for the golang image and go.mod's `go` directive, so CI and the image use the same Go. The `go`
directive now actually bumps; under the fleet's `rangeStrategy: pin` it never did. Go patches automerge; a Go minor
(1.27 -> 1.28) opens as a manual-review PR with a 7-day soak. While that minor PR is open no Go patch PR opens, because
the shared preset always targets the newest release. The image then misses the security releases Go ships for the
previous minor, so merge the minor soon after reviewing it. `deploy/compose.yaml` is an example and is ignored, so it
keeps the floating `latest` tag.

The zebrash rule: `github.com/Gobbo89/zebrash`, plus `github.com/ingridhq/gg` and `github.com/ingridhq/maxicode` (still
pulled from the abandoned upstream org), get their own group, no automerge and a `needs-review` label.
Vulnerability-fix PRs are the exception: the shared `vulnerabilityAlerts` config automerges them and is applied with
`force`, which a packageRule can't override.

Advisories don't follow the fork. The Go vulnerability database, GHSA and Trivy match by module path, so a CVE filed
against `github.com/ingridhq/zebrash` won't flag `github.com/Gobbo89/zebrash`, although most of its code is the same.
When reviewing a fork update, or every few months, check upstream's advisories
(github.com/ingridhq/zebrash/security and the Go vuln DB entry for that path) by hand.
