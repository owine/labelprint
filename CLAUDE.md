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
| `render.go` | `pdfinfo`/`pdftoppm` wrapper: page geometry, page and box renders, path B (render to the label's dots), scratch files on the `/tmp` tmpfs |
| `geom.go` | `box` (fractions of the page as displayed), page geometry, the pdftohtml offset table, `crops`/`layout` parsing |
| `crop.go` | label detection on a 50 dpi ink mask: XY-cut, label-sized test, frame stripping, UPS doc-tab split |
| `native.go` | path A: embedded bitmaps printed 1:1 (pdfimages gate, pdftohtml positions, verified against a render) |
| `labels.go` | per page: which boxes (and the skip rule), and each box's label (path A, else B) |
| `bitmap.go` | gray-image helpers: threshold, rotate, trim, centre |
| `zpl.go` | ZPL preview via zebrash (the only module dependency, `github.com/Gobbo89/zebrash`): panic recovery, timeout, canvas from `^PW`/`^LL` or the media |
| `media.go` | turns LPrint's `media-supported` PWG names into dropdown labels and sizes |
| `server.go` | HTTP handlers, upload limits, the print flow, cross-origin protection, security headers |
| `maintenance.go` | `POST /api/printer/maintenance`: "feed a label" (a one-dot box) and "test label" as ZPL jobs. No remote calibrate: that's SmartCal at the printer |
| `testlabel.go` | the darkness/speed test label: server-side ZPL packed into the selected media, leaving out what doesn't fit; never sets `~SD`/`^MD`/`^PR` |
| `share.go` | phone sharing: `POST /share` holds a checked upload in memory under a one-time token for the page's preview |
| `web/` | the page: vanilla JS, owine design tokens (`tokens.css`), vendored fonts. No build step |
| `shortcut/` | source (XML plist) of the signed iOS Shortcut served as `web/labelprint.shortcut`; see below |

## Constraints that shaped the code

Verified against the live LPrint (Get-Printer-Attributes) and the LPrint/PAPPL sources:

- LPrint accepts `application/vnd.zebra-zpl`, `image/png`, `image/jpeg`, `image/pwg-raster`, `image/urf`. **No PDF**,
  so PDFs are rasterised here.
- `multiple-document-jobs-supported = false`, so it's one Print-Job per PDF label.
- `page-ranges` isn't a job-creation attribute, so ranges are applied at render time.
- LPrint's ZPL driver (`lprint_zpl_printfile`) copies raw data through once and **ignores `copies`**. ZPL copies are
  made by repeating the document in a single job. Don't "fix" this by sending `copies`.
- PAPPL's image path does honour `copies`, `print-scaling` and auto-rotation, so PNG/JPEG/PDF pages rely on them.
- PDF labels are thresholded to pure black/white at the printer's dots, so PAPPL's dither never sees gray.
- Labels never enlarge: path B scales by `min(1, fit)` and path A is 1:1. Carriers ask for true size. The page warns
  when a label shrinks below 95% / 87%.
- Detection counts anything under gray 240 as ink, so hairline label borders survive the 50 dpi render. A thin frame
  round a label image is stripped, but only at a dead end (no gutters, not label-sized); a doc tab is at most 0.2x
  the media's long side.
- In `layout=auto`, a page with no label is skipped when other selected pages have labels. Pages the user gave
  `crops` for count as having labels and are never skipped.
- Native passthrough (path A) only applies after the bitmap matches a render of the box, and it never scales.
  Verification is local (a sliding window, plus a check for stray ink beside the image), runs at the image's own
  resolution, and refuses when anything else is drawn in the box (e.g. a generator's logo over the bitmap). That's on
  purpose: falling back to a render is safe, printing hidden content is not.
- pdftohtml clamps `-zoom` to 3 and reports positions relative to the MediaBox (see `htmlOffset`).
- Logs carry no poppler output or label text, only page numbers and fixed reasons.
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

## The iOS Shortcut

`web/labelprint.shortcut` is a signed build of `shortcut/labelprint.shortcut.xml` (the XML lives outside `web/` so it
isn't served). Actions 0 and 1 are Texts holding `https://labelprint.example` and `paste-your-share-key`, which two
import questions (`ActionIndex` 0 and 1, `ParameterKey` `WFTextActionText`) replace with the user's address and share
key. Import questions can only fill plain action parameters, hence the Texts. The rest POSTs Shortcut Input to
`<address>/share` as form field `file`, with headers `Accept: application/json` and `X-Labelprint-Share: <key>`, and
opens `<address>` + the reply's `url`. If you insert or reorder actions, fix the questions' `ActionIndex`. labelprint
never checks the share key: the auth proxy matches it (README, "Letting the shortcut past your auth proxy"), so don't
add an app-side check. A key left as the placeholder is sent harmlessly. Edit the XML, never the signed file, and keep
real addresses and keys out of it. Rebuild:

```sh
plutil -lint shortcut/labelprint.shortcut.xml
plutil -convert binary1 -o /tmp/unsigned.shortcut shortcut/labelprint.shortcut.xml
shortcuts sign --mode anyone --input /tmp/unsigned.shortcut --output web/labelprint.shortcut
```

Signing needs a Mac signed in to iCloud. The signed file is an AEA archive whose only identity is a fresh
Apple-issued certificate with a random CN, but check it (`strings web/labelprint.shortcut`) for a name, email or
address before committing a new build. The certificate expires about a year after signing, so re-sign if iOS starts
refusing the import.

## Commands

```sh
go test -race ./...                                  # PDF tests skip without poppler
UPDATE_FIXTURES=1 go test -run Fixture               # after changing makeLabelPDF
go test -run '^$' -fuzz '^FuzzZPLPreview$' -fuzztime 5m .  # crashers land in testdata/fuzz/
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
