# labelprint

A one-page web app for printing shipping labels on a Zebra label printer. Drop in a ZPL, PDF, PNG or JPEG file
(or paste one, or a screenshot), check the preview, pick copies and a label size, and press print. The page remembers
the label size and copies in your browser; copies go back to 1 after each successful print. Every job goes to
[LPrint](https://www.msweet.org/lprint/) over IPP, so it shows up in LPrint's own job list. Previews and PDF rendering
happen inside the container; nothing is sent to an outside service, which matters for labels full of names and
addresses.

```
browser ──▶ reverse proxy + auth ──▶ labelprint (container) ──IPP Print-Job──▶ LPrint ──▶ Zebra printer
```

Built and tested against a Zebra GX420t (203 dpi) on LPrint, with 4×6 in shipping labels. Other printers LPrint
drives should work; set `RENDER_DPI` to match.

> [!IMPORTANT]
> labelprint has **no login of its own**. Anyone who can reach it can print. Put it behind an authenticating proxy
> or keep it on a network only you can reach. See [SECURITY.md](SECURITY.md).

## What it does with each file

| file | how it's detected | what LPrint receives |
|---|---|---|
| ZPL | `.zpl`, or any file containing `^XA` … `^XZ` (`.txt` must contain one) | the bytes, unchanged, as `application/vnd.zebra-zpl` |
| PDF | `%PDF-` header | one job per page: an 8-bit grayscale PNG rendered at 203 dpi, `print-scaling=fit` and the chosen `media` |
| PNG / JPEG | magic bytes | the file unchanged, `print-scaling=fit` and the chosen `media` |

Anything else is rejected, and so are uploads over 20 MB.

Some details that aren't obvious:

- **ZPL copies are made here.** LPrint's ZPL driver sends raw ZPL straight to the printer and ignores the IPP `copies`
  attribute. For 3 copies, labelprint sends one job containing the ZPL three times. ZPL jobs carry no `media`
  either, because the ZPL sets its own size (`^PW`, `^LL`).
- **PDFs become one job per page.** LPrint takes a single document per job (`multiple-document-jobs-supported` is
  false). Page ranges (`1-3, 5`) are applied while rendering, because LPrint doesn't support `page-ranges`.
  With copies, each page's job carries `copies=N`, so a 2-page PDF with 3 copies prints page 1 three times, then
  page 2 three times.
- **Rendering has anti-aliasing turned off** (`pdftoppm -aa no -aaVector no`). Gray barcode edges would come out of
  LPrint's 1-bit dither fuzzy; hard edges at 203 dpi map one-to-one to printer dots.
- **Fonts that PDFs reference but don't embed**, as carrier labels often do with Helvetica, are drawn with Liberation
  Sans/Serif/Mono.
- **"Sent" isn't "printed."** LPrint accepts a job even when the printer is offline or out of media and holds it.
  After printing, the page polls Get-Jobs. A job that ends up aborted or canceled turns the result red and shows
  LPrint's `job-state-message`, and the header shows the printer's state reasons. A job that's still pending, held,
  processing or stopped can be canceled from the recent-jobs table (click "cancel", then "confirm?"), so a wrong
  label doesn't print when the printer comes back. LPrint's answer shows in the row.

The preview is drawn in the container too, for up to 12 pages or labels:

- **ZPL** is drawn by [zebrash](https://github.com/Gobbo89/zebrash), a pure-Go emulation of the ZPL engine that
  handles MaxiCode, PDF417, Code 128, EAN-8/13, `^GF` graphics and more. That's the maintained fork: the original
  [ingridhq/zebrash](https://github.com/ingridhq/zebrash) stopped getting updates in 2026, and the fork carries its
  bug fixes and limits for malformed ZPL. It's an emulation, so fonts can differ slightly from
  the printer's; the printer always gets the original ZPL. The canvas uses the label's own `^PW`/`^LL` when set, and
  the size dropdown otherwise, which is the dropdown's only job for ZPL. Labels drawn with `^POI` (printed rotated
  180°) are shown upright. If zebrash can't draw a label, the page says why and shows the source; printing isn't
  affected.
- **PDFs** show the pages exactly as they'll be sent: 203 dpi grayscale.
- **PNG and JPEG** previews are drawn by your browser.

## Feeding and calibrating

**Feed a label** in the header feeds one blank label. It's sent as an ordinary ZPL job through LPrint, so it shows up
in LPrint's job list. The ZPL is fixed on the server (a one-dot box, `^XA^FO0,0^GB1,1,1^FS^XZ`), so the endpoint
can't be used to send arbitrary ZPL.

**Calibration happens at the printer.** If it reports `media-empty` with labels loaded, use SmartCal: hold FEED until
the light flashes twice, then let go. There's no remote calibrate on purpose. Sending `~JC` through LPrint left the
printer jammed, and nobody was there to clear it.

## Sharing from a phone

Return labels usually arrive by email. Instead of saving the attachment and uploading it, share it straight to
labelprint. A share always lands on the preview: you check it and press print. Nothing prints from a share alone.

Both platforms use the same hand-off. The phone POSTs the file to `/share`, which checks it like an upload and holds
it in memory under a one-time link (`/?share=<token>`). The page collects it from there and shows the preview. A
shared file is never written to disk. It's dropped once the page has collected it, or after 10 minutes. labelprint
holds at most 8 shares and 80 MB in total (more only if `MAX_UPLOAD_MB` is higher, so one upload always fits), and
drops the oldest share to make room.

**Android (Chrome).** Open labelprint in Chrome and choose **Add to Home screen** (or **Install app**) from the menu.
It needs HTTPS. The installed app then appears in the share sheet for PDFs, PNGs, JPEGs and `.zpl`/`.txt` files. The
installed app shares Chrome's cookies, so stay signed in to your auth proxy. If the proxy sends the share to a login
page, the file is lost, so open the app once to sign in, then share again.

**iPhone (Shortcuts).** Safari can't be a share target, so use a Shortcut. labelprint ships a ready-made one:

1. Open labelprint in Safari on the iPhone and tap **add the share shortcut to an iPhone** at the bottom of the page.
   Safari downloads `labelprint.shortcut`; open it from the downloads list.
2. Choose **Add Shortcut** and answer the one question with your labelprint's address, e.g.
   `https://labels.example.com` (no trailing slash).
3. Share a label (PDF, image or `.zpl`) from Mail or Files and pick **labelprint**. Safari opens the preview.

The shortcut POSTs the file to `/share` with `Accept: application/json`. `/share` then answers
`{"url": "/?share=<token>"}` instead of redirecting, and errors come back as `{"error": "…"}`, which the shortcut
shows. Your auth proxy has to let the shortcut through (see below). If it wants a header or token, edit the
shortcut and add it to **Get Contents of URL**.

<details>
<summary>Building the shortcut by hand</summary>

1. Create a shortcut. In its details, turn on **Show in Share Sheet** and accept **PDFs**, **Images** and **Files**.
2. **Get Contents of URL**: `https://<your labelprint>/share`, method **POST**, request body **Form** with one
   field set to **Shortcut Input**. Add a header `Accept: application/json`, plus whatever your auth proxy needs.
3. **Get Dictionary Value** `url` from **Contents of URL**. It's a path like `/?share=…`.
4. **Text**: `https://<your labelprint>` followed by **Dictionary Value**, then **Open URLs** with that text.

Two details are easy to get wrong. The form field must be of type **File** (not Text) and named `file`. The
dictionary key must be the literal text `url`, typed in, not a variable.

</details>

> [!NOTE]
> A Shortcut doesn't carry Safari's cookies, so your auth proxy needs a rule for `POST /share` before the shortcut
> works. How depends on the proxy. Options include a header or token the proxy accepts for this one path, or a rule
> that passes `POST /share` from your LAN or VPN without a login. Anyone the proxy lets through can stage files. They
> can't print, and they can't read a share without its link, but they can push your pending share out early by
> filling the 8 slots. Opening the link in Safari still goes through your normal login.

## Configuration

All settings are environment variables. Only `LPRINT_URI` is required.

| variable | default | |
|---|---|---|
| `LPRINT_URI` | *(required)* | the LPrint queue, e.g. `ipp://lprint-host:8000/ipp/print/Zebra-GX420t` (`ipp://` or `ipps://`; port 631 if none is given). labelprint won't start without it |
| `DEFAULT_MEDIA` | `na_index-4x6_4x6in` | preselected size. Falls back to LPrint's `media-default` if LPrint doesn't list it |
| `MAX_UPLOAD_MB` | `20` | upload cap |
| `MAX_PAGES` | `50` | most PDF pages one print may send |
| `MAX_COPIES` | `100` | |
| `RENDER_DPI` | `203` | PDF and ZPL preview resolution. Match the printer |
| `USER_HEADER` | *(off)* | a request header your auth proxy sets to the signed-in user (Authelia: `Remote-User`), shown as the job's user in LPrint. Only set it if the proxy always overwrites that header |
| `LISTEN_ADDR` | `:8080` | |

The size list comes from LPrint's `media-supported`; the `roll_max`/`roll_min` range entries are left out. Sizes in
`media-ready` are marked "loaded".

## Running it

The image is `ghcr.io/owine/labelprint` (linux/amd64 and linux/arm64). Pin a release as `X.Y.Z@sha256:…`; `latest`
follows releases and `main` follows the main branch.

[`deploy/compose.yaml`](deploy/compose.yaml) is an example stack behind Traefik with an auth middleware. Change
`LPRINT_URI`, the `Host` rule and the middleware to suit. The parts worth keeping:

```yaml
services:
  labelprint:
    image: ghcr.io/owine/labelprint:latest
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    tmpfs:
      - /tmp:size=128m        # a PDF and its rendered pages, briefly
    mem_limit: 512m
    environment:
      LPRINT_URI: "ipp://lprint-host.lan:8000/ipp/print/Zebra-GX420t"
    healthcheck:
      test: ["CMD", "/usr/local/bin/labelprint", "healthcheck"]
```

Or, to try it on its own:

```sh
docker run --rm -p 8080:8080 --read-only --cap-drop ALL --tmpfs /tmp \
  -e LPRINT_URI=ipp://lprint-host.lan:8000/ipp/print/Zebra-GX420t ghcr.io/owine/labelprint:latest
```

Notes:

- **Reaching LPrint.** The container logs `lprint reachable` (or the error) at startup. If LPrint runs on the Docker
  host, normal bridge networking usually reaches it at the host's LAN address. If it doesn't, check that LPrint listens
  on that address and that the host firewall lets Docker's subnets reach LPrint's port. Failing that, add
  `extra_hosts: ["host.docker.internal:host-gateway"]` and use `ipp://host.docker.internal:<port>/ipp/print/<queue>`.
  That route only works if LPrint listens on the bridge address too.
- **Hardening.** The container runs as uid 65532 with a read-only root and all capabilities dropped. Uploads are held
  in memory, and a PDF sits on the `/tmp` tmpfs only while it renders. The healthcheck only tests this process: LPrint
  being down shows in the page, and isn't a reason to restart the container.
- **Cross-site requests.** An auth proxy's cookie goes with every request to the host, so POSTs from other sites are
  refused using Go's `http.CrossOriginProtection`. Same-origin pages and plain `curl` work.

## Developing

You need Go (version in `go.mod`), and poppler (`brew install poppler`) for the PDF tests, which skip without it.

```sh
go test -race ./...
LPRINT_URI=ipp://lprint-host.lan:8000/ipp/print/Zebra-GX420t LISTEN_ADDR=127.0.0.1:8080 go run .
docker buildx build --platform linux/arm64 -t labelprint:test --load .
```

The test fixtures are synthetic. `testdata/label-2page.pdf` is generated by `makeLabelPDF` in `helpers_test.go`
(regenerate it with `UPDATE_FIXTURES=1 go test -run Fixture`), and `testdata/sample.zpl` is a made-up shipping label
that exercises the same features as real carrier ZPL. Never commit real labels: they contain names and addresses,
and `.gitignore` excludes `*.zpl` outside `testdata/`.

PR titles are [conventional commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `deps:` …); they drive
the release notes. CI builds and smoke-tests both architectures on every PR. On `main` it publishes `:sha-<commit>`
and `:main`. Publishing a GitHub release retags that same image as `X.Y.Z`, `X.Y`, `X` and `latest`, without
rebuilding.

## License

MIT, see [LICENSE](LICENSE). The bundled web fonts are under the SIL Open Font License: Geist and Geist Mono
([`OFL-geist.txt`](web/fonts/OFL-geist.txt), [`OFL-geistmono.txt`](web/fonts/OFL-geistmono.txt)) and Instrument
Serif ([`OFL-instrumentserif.txt`](web/fonts/OFL-instrumentserif.txt)). ZPL previews use
[zebrash](https://github.com/Gobbo89/zebrash) (MIT), the maintained fork of
[ingridhq/zebrash](https://github.com/ingridhq/zebrash).
