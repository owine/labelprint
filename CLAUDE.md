# CLAUDE.md

Guidance for Claude Code in this repository. User-facing behaviour and configuration live in `README.md`; this file
covers how the code is built and the constraints that aren't visible from it.

**Versions are not restated here.** Go, the base images and every apk pin live in `go.mod` and the `Dockerfile`. The
current release is the latest published GitHub Release (`gh release view`).

## What it is

A single Go binary that serves an embedded page (`web/`) and talks IPP to LPrint, which runs
on the Docker host (not in a container) and drives the label printer; developed against a Zebra GK420t. It
runs behind a reverse proxy with an auth layer, since it has no login of its own. `deploy/compose.yaml` is an
example stack. The repo is public: keep real hostnames, addresses and deployment details out of it.

| file | role |
|---|---|
| `ipp.go` | hand-written IPP/2.0 encoder/decoder and the three operations used: Print-Job, Get-Printer-Attributes, Get-Jobs |
| `detect.go` | content sniffing (PDF/PNG/JPEG magic, ZPL by `.zpl` or `^XA…^XZ`), page-range parsing |
| `render.go` | `pdfinfo` + `pdftoppm` wrapper; one call per page, scratch files on the `/tmp` tmpfs |
| `zpl.go` | ZPL preview via zebrash (the only module dependency): panic recovery, timeout, canvas from `^PW`/`^LL` or the media |
| `media.go` | turns LPrint's `media-supported` PWG names into dropdown labels and sizes |
| `server.go` | HTTP handlers, upload limits, the print flow, cross-origin protection, security headers |
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
- zebrash has no panic recovery of its own and is fed untrusted uploads, so `zpl.go` recovers and times out around
  it. A failed ZPL preview is reported in the page (`previewError`) and must never block printing.
- The printer can report `media-empty` while printing fine, so state reasons are shown as warnings and never block a
  print.

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

Renovate extends the `owine/renovate-config` presets (base, automerge, docker, alpine). The apk pins track the Alpine
line set by `branch=v3.XX` in `.github/renovate.json`. When the Alpine base crosses a release line, bump the base tag
and digest, every apk pin, and that `branch=` in one PR. Go modules are tidied on update (`gomodTidy`). The golang image and go.mod's `go` directive are grouped so CI
and the image use the same Go. `deploy/compose.yaml` is an example and is ignored, so it keeps the
floating `latest` tag.
