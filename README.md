# labelprint

A one-page web app for printing shipping labels on a Zebra label printer. Drop in a ZPL, PDF, PNG, JPEG or GIF file
(or several, or paste one, or a screenshot), check the preview, pick copies and a label size, and press print. The
page remembers the label size and copies in your browser; copies go back to 1 after each print that sends something.
Every job goes to
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
| PDF | `%PDF-` header | one job per label: the label boxes found on each page (or chosen in the preview), each a pure black/white PNG exactly the label's size, `print-scaling=fit` and the chosen `media` |
| PNG / JPEG / GIF | magic bytes | as for a PDF: the image is taken as a one-page PDF (see [Images](#images)), so each label is a pure black/white PNG exactly the label's size, `print-scaling=fit` and the chosen `media` |

Anything else is rejected, and so are uploads over 20 MB. EPL2 (the other format UPS and FedEx offer, recognised by an
`N` line followed by commands such as `q`, `Q`, `A`, `B` or `P`) is refused with a message to choose ZPL or PDF in your
shipping tool, even if the file is named `.zpl`: LPrint doesn't accept EPL2.

Some details that aren't obvious:

- **ZPL copies are made here.** LPrint's ZPL driver sends raw ZPL straight to the printer and ignores the IPP `copies`
  attribute. For 3 copies, labelprint sends one job containing the ZPL three times. ZPL jobs carry no `media`
  either, because the ZPL sets its own size (`^PW`, `^LL`).
- **PDFs become one job per label.** LPrint takes a single document per job (`multiple-document-jobs-supported` is
  false). Page ranges (`1-3, 5`) pick the pages first, since LPrint doesn't support `page-ranges`. With copies,
  each label's job carries `copies=N`, so a 2-up page with 3 copies prints the first label three times, then the
  second three times.
- **Labels are pure black and white at the printer's dots.** A label is rendered to the label size at the printer's
  resolution with anti-aliasing off, then thresholded. LPrint's dither has nothing to do, so barcode edges stay
  crisp instead of speckled. A label is never enlarged: one smaller than the media prints at its own size, centred.
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
  the size dropdown otherwise. Labels drawn with `^POI` (printed rotated 180°) are shown upright. If zebrash can't
  draw a label, the page says why and shows the source; printing isn't affected.
- **ZPL that doesn't fit** gets a warning above the preview: when the largest `^LL` in the file is more than 2%
  longer than the label size in the dropdown at `RENDER_DPI` (a 4×8 or 4×6.75 doc-tab label on 4×6 stock), the
  largest `^PW` is wider by more than 2% or 0.12 in, whichever is larger (so a head-width `^PW832` on 4 in labels
  passes), or the `^PW` looks like a 300 dpi label on a 203 dpi printer. The ZPL is still sent unchanged and print
  stays enabled; ZPL without `^PW`/`^LL` isn't checked.
- **PDFs** show each page at 96 dpi with its label boxes drawn over it, and the labels cut from those boxes exactly as
  they'll be sent: black and white at `RENDER_DPI`, turned and fitted to the label size.
- **Images** (PNG, JPEG, GIF) are previewed like a one-page PDF: the image with its label boxes, the labels exactly
  as they'll be sent, and a note above saying how big the image was taken to be.

## Label area (PDFs)

Store and carrier PDFs often put the label on a Letter or A4 sheet, beside instructions, sometimes two to a
page. labelprint finds the label-sized blocks on each page and prints each as its own label:

- **auto** (the default) splits each page at its white gutters and keeps blocks between 0.6x and 1.2x the label
  size. It sees through a thin frame drawn round a label image. A page with nothing label-sized prints whole. A
  block of instructions that happens to be label-sized shows up as an extra box; delete it.
- **whole page** prints each page as one label.
- **Skipped pages.** Pages with no label found are left out when other pages have labels (instruction pages in a
  multi-page PDF). The preview shows them as not printing, with **include** to print one anyway. If no page has a
  label, they all print whole.
- In the preview you can move, resize, add and remove boxes, or reset a page. Edits apply to that file only.
- **Upside-down labels.** Senders don't agree which way up a landscape label goes, so one can come out upside down.
  **flip** under a label in the preview turns it 180°. A doc tab printed separately turns with its label. Moving the
  box keeps the flip; like any edit, reset, removing the box, or changing the pages, label size or label area drops
  it.
- **Shrink warnings.** Labels print at true size or smaller. When one has to shrink to fit, the page says so: below
  95% it warns that barcodes may not scan, and below 87% the warning is stronger. Nothing is blocked.
- **Carrier bitmaps print unscaled.** When a label is a black/white bitmap at about the printer's resolution (UPS
  labels usually are), its own pixels are printed 1:1. This only happens after checking them against a render of
  the page, both over the whole box and locally, so a stamp, a logo or text drawn over or beside the image falls
  back to rendering.
- **UPS doc tabs.** Some UPS return labels have a strip after the label (up to 1.2 in) carrying the retailer's own
  barcode. It doesn't fit 4x6 at full size, so the preview asks: leave it off (the default), include it (all
  scaled to fit), or print it as a separate label.
- **Printing finds the labels again.** If that fails at print time (an overloaded host, say), nothing is sent and
  the page says so: try again, or choose whole page. Falling back would print whole pages the preview didn't show.
- **Known limit.** A label-sized block of instructions can't be told from a label, and appears as an extra box.
- **Large-format pages are refused.** A page or box that would be over 16 megapixels at `RENDER_DPI` (about
  19.7 in square at 203 dpi; Tabloid is well inside) gets "too large to print as a label", to keep memory bounded.

## Several files

A multi-piece shipment comes as one label file per package: ShipSaving sends a PNG each, and FedEx a label per
package. The page holds a queue of up to 10 files and prints them in one go.

- **Adding.** Choose or drop several files at once, or paste one. Files are added to the end of the queue, never
  replaced: **choosing a second file adds it, it doesn't swap out the first.** The list's header ("2 files") and the
  button ("print 2 labels") show it; **remove** takes a file out, and **clear all** (press it twice) empties the queue.
  Past 10 files, as many as fit are added and the page says how many weren't. Empty files and folders in a drop are
  skipped. A file over the size limit, or one labelprint can't print, stays in the list marked failed, with the
  reason, until you remove it.
- **One file open at a time.** Each row shows the file's kind, labels, copies and a ⚠ for anything to look at (a
  label that shrinks below 95%, a barcode that couldn't be read, ZPL bigger than the labels, a refused page range).
  Pick a file's name to open it in the editor below the list.
- **Per file:** copies, page range, label area, box edits, flips and doc-tab choices. **Shared:** the label size, since
  there's one roll in the printer. Changing it re-previews every file and clears their edits, and so does LPrint
  dropping the size in use (the page says so).
- **Barcode checks run for every file** in the background, one at a time, not only for the open one. The line by the
  print button counts the labels whose barcode couldn't be read and the labels not checked yet.
- **print N labels** sends every file, in list order, one after another. While it runs the page is locked: the list,
  adding files, the label size, the editor, feed and the test label. A failure doesn't stop the rest, and the result
  lists each file: sent (with its jobs), failed (with the reason) or partly sent.
  - Files that printed leave the queue, so what's left is what didn't. A file that failed before anything was sent
    (LPrint down, say) stays and is sent again by the next print.
  - A file that failed after sending some of its jobs is marked **partly sent**. The next print asks before sending
    all of it again; change its page range first to send only the rest.
- **add back** on a printed file's line puts it back in the queue with its edits, to print again after a jam. It
  works once per line.
- **Copies** work as before: new files start from the last count you typed, and it goes back to 1 once a print has
  sent something.
- **A phone share starts a new page** holding just that file, since the queue isn't kept across reloads. An installed
  Android app that reuses its window would drop a queue you were building (untested); the iPhone shortcut opens its
  own Safari tab.

## Images

Labels come as images too: ShipSaving sends PNGs, the UPS API GIFs, and FedEx 800 × 1200 PNGs at 200 dpi. An
image goes through the same pipeline as a PDF, as a one-page PDF of its own. That gives it the label boxes and
the editor, the UPS doc-tab choice, unscaled printing of a black/white label at about the printer's resolution,
and the shrink warnings. A label is never enlarged. A gray or colour image (a screenshot, a JPEG) is thresholded
to black and white, so LPrint's dither never sees it. An image gets the flip toggle too.

**How big an image is.** An image has pixels, not inches, so labelprint works out its resolution (ppi), in
order:

1. The PNG's own `pHYs` density or the JPEG's JFIF density, when it's between 100 and 600 ppi. 72 and 96 are
   usually placeholders, so they're ignored.
2. The label size. When the image has the label's shape (within 3%), it's taken to be exactly the label's size. A
   label plus a 1 in doc tab (4 × 7 in for 4 × 6 labels) counts too. This allows 50 to 600 ppi, so a 96 ppi
   screenshot of a 4 × 6 label still comes out at 4 × 6 in.
3. Otherwise, `RENDER_DPI`.

The preview says which, above the image: "800 × 1200 px at 200 ppi (from the file) → 4 × 6 in" or "… (matched to
the 4 × 6 in labels) …". When it had to guess, the note is a warning: "300 × 300 px (size guessed at 203 ppi —
check it) → 1.48 × 1.48 in". A matched size can change with the label size, so changing it redraws the preview.
If a guess is wrong, draw the box yourself or choose **whole image**.

**Limits.** These are checked from the file's header before it's decoded, so an image can't tie up the host:

- at most 4000 px on a side;
- what decoding would take in memory, worked out from the header: at most 96 MB (two can run at once);
- at most 64 scans in a JPEG (a progressive JPEG has about 10);
- at least 0.5 in on a side at its ppi, and under the 16-megapixel cap at `RENDER_DPI`.

Only an animated GIF's first frame is printed. EXIF rotation is ignored, and photos of labels aren't supported.
TIFF, WebP and BMP aren't accepted.

## Barcode check

For a PDF or an image, the preview checks each label's barcode in the exact black-and-white image LPrint will get.
Each label gets a line under it:

- **UPS · 1Z… · barcode reads ✓**: a barcode decodes, and its value has a carrier's tracking-number format.
- **barcode reads, no tracking number recognised**: a barcode decodes, but no value is a tracking number. It might
  be a reference number, a postal routing barcode, or the QR code on a return label.
- **⚠ no barcode could be read — check this label before printing**: nothing decodes. This is the one to look at:
  a label shrunk too far to fit, a box edited across the barcode, or a doc tab included on 4×6 stock.
- **barcode check didn't finish**: the check timed out or was dropped.

It's a hint: printing is never blocked, whatever it says. The summary beside **print** counts the labels whose
barcode couldn't be read.

**What's checked.** Code 128 (GS1-128 included) and Code 39 barcodes, and QR codes, which count as "reads".
MaxiCode, DataMatrix and PDF417 aren't checked. Tracking numbers are recognised by their format only, without check
digits:

- UPS `1Z…`;
- USPS IMpb: `92`–`95` and 22 or 26 digits, alone or after a `420` ZIP;
- FedEx Ground: `96…`, shown as its last 15 or 12 digits;
- FedEx Express: 34 digits, shown as the last 12.

The FedEx lengths and the slices shown come from third-party descriptions of the formats. They haven't been checked
against a FedEx-published specification, so treat a FedEx line as a best guess.

USPS's legacy formats (`91…`, 20 digits) aren't recognised. Nor are plain 12- or 15-digit numbers: they look just
like the reference numbers most labels carry.

**How it runs.** The check runs in the container with [gozxing](https://github.com/makiuchi-d/gozxing). It starts
when the page shows the preview and works through the labels one at a time in the background, at most 2 s each. It
covers the labels the preview shows, up to 12. Tracking numbers stay in the page: they aren't logged, kept in the
browser or put in the job name, and the server holds them in memory for at most 10 minutes. The page collects them
from `/api/check/<token>`, so a reverse proxy's access log records those tokens, as it does share tokens. That's
harmless behind your auth proxy, with a 10-minute lifetime.

**ZPL** isn't decoded: the printer draws its barcodes itself. Instead, the preview reads the tracking number from
the ZPL's own `^BC` (Code 128) and `^B3` (Code 39) fields and shows "tracking · UPS · 1Z… (from the zpl)", with no
✓. Fields written with `^FH` hex escapes or `^FN` templates are skipped.

## Feeding, test label and calibrating

**Feed a label** in the header feeds one blank label. It's sent as an ordinary ZPL job through LPrint, so it shows up
in LPrint's job list. The ZPL is fixed on the server (a one-dot box, `^XA^FO0,0^GB1,1,1^FS^XZ`), so the endpoint
can't be used to send arbitrary ZPL.

**Test label** prints a reference for tuning darkness and speed: PDF417 with a 2-dot module, Code 128 at 2, 3 and 4
dots, a MaxiCode, a grid of 1-dot lines and a solid block. Like feed, it's ZPL generated on the server, laid out for
the label size picked in the page. Smaller labels leave out what doesn't fit, and a 2×1 still gets the PDF417 and
the grid. The label sets no darkness or speed of its own (the 4×6 version is in `testdata/test-label-4x6.zpl`).

To tune, set darkness and speed once in LPrint's web UI. LPrint sends them to the printer with PDF and image jobs
only, so print a PDF or image after a change, then the test label. Then check:

- the 2-dot PDF417 and the 1-dot grid are crisp, with every line there and no gaps filled in;
- the block is solid black, with no pale streaks;
- the Code 128 bars haven't spread into each other.

Too light breaks lines and leaves the block grey; too much heat (or too much speed for the heat) spreads the bars,
which hits the 2-dot PDF417 and the MaxiCode first. FedEx asks for at least 90% print contrast on direct-thermal
labels, so go as dark as you can before the bars start to spread.

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
It needs HTTPS. The installed app then appears in the share sheet for PDFs, PNGs, JPEGs, GIFs and `.zpl`/`.txt` files. The
installed app shares Chrome's cookies, so stay signed in to your auth proxy. If the proxy sends the share to a login
page, the file is lost, so open the app once to sign in, then share again.

**iPhone (Shortcuts).** Safari can't be a share target, so use a Shortcut. labelprint ships a ready-made one:

1. Open labelprint in Safari on the iPhone and tap **add the share shortcut to an iPhone** at the bottom of the page.
   Safari downloads `labelprint.shortcut`; open it from the downloads list.
2. Choose **Add Shortcut** and answer its two questions: your labelprint's address, e.g. `https://labels.example.com`
   (no trailing slash), and your share key (see below). If your proxy doesn't need a key, leave the second answer
   as it is.
3. Share a label (PDF, image or `.zpl`) from Mail or Files and pick **labelprint**. Safari opens the preview.

The shortcut POSTs the file to `/share` with the headers `Accept: application/json` and
`X-Labelprint-Share: <your share key>`. `/share` then answers `{"url": "/?share=<token>"}` instead of redirecting,
and errors come back as `{"error": "…"}`, which the shortcut shows. To change the key later, edit the shortcut: it's
the second **Text** action.

<details>
<summary>Building the shortcut by hand</summary>

1. Create a shortcut. In its details, turn on **Show in Share Sheet** and accept **PDFs**, **Images** and **Files**.
2. **Get Contents of URL**: `https://<your labelprint>/share`, method **POST**, request body **Form** with one
   field set to **Shortcut Input**. Add the headers `Accept: application/json` and `X-Labelprint-Share` with your
   share key.
3. **Get Dictionary Value** `url` from **Contents of URL**. It's a path like `/?share=…`.
4. **Text**: `https://<your labelprint>` followed by **Dictionary Value**, then **Open URLs** with that text.

Two details are easy to get wrong. The form field must be of type **File** (not Text) and named `file`. The
dictionary key must be the literal text `url`, typed in, not a variable.

</details>

### Letting the shortcut past your auth proxy

A Shortcut doesn't carry Safari's cookies, so your auth proxy has to let `POST /share` through without a login. The
share key keeps that opening narrow: the proxy skips the login only for `POST /share` requests that carry the key in
`X-Labelprint-Share`. labelprint doesn't check the header itself; the proxy is the gate.

1. Generate a key with `openssl rand -base64 32`.
2. Put it in the stack's `.env` as `LABELPRINT_SHARE_KEY=<key>`.
3. Type the same key into the shortcut's second question when you add it.

With Traefik v3, keep your normal router and add a second one without the auth middleware. Its priority makes Traefik
try it first:

```yaml
labels:
  traefik.http.routers.labelprint.rule: "Host(`labels.example.com`)"
  traefik.http.routers.labelprint.middlewares: "authelia@file"
  traefik.http.routers.labelprint-share.rule: >-
    Host(`labels.example.com`) && Method(`POST`) && Path(`/share`)
    && Header(`X-Labelprint-Share`, `${LABELPRINT_SHARE_KEY:?}`)
  traefik.http.routers.labelprint-share.priority: "100"
  traefik.http.routers.labelprint-share.service: "labelprint"
  traefik.http.services.labelprint.loadbalancer.server.port: "8080"
```

Compose fills in `${LABELPRINT_SHARE_KEY:?}` from `.env`, and the `:?` stops it starting with an empty key, which
would let through any request that sends the header empty. Traefik v2 spells the matcher `Headers`. A request with a
wrong or missing key falls through to the normal router and its login, and the shortcut shows the error.

Other proxies work the same way: a rule that passes `POST /share` without a login only when `X-Labelprint-Share`
matches the key. If you use Authelia, put that rule in the proxy in front of it. Authelia's own access-control rules
can't match request headers. A rule that passes `POST /share` from your LAN or VPN is another option.

Either way, a client that gets past the rule can only stage files. It can't open a share without its link, can't
print, and can't read anything back. At worst it can churn the store (8 shares, 80 MB) and push your pending share out
early. Opening the link in Safari still goes through your normal login.

## Configuration

All settings are environment variables. Only `LPRINT_URI` is required.

| variable | default | |
|---|---|---|
| `LPRINT_URI` | *(required)* | the LPrint queue, e.g. `ipp://lprint-host:8000/ipp/print/Zebra-GX420t` (`ipp://` or `ipps://`; port 631 if none is given). labelprint won't start without it |
| `DEFAULT_MEDIA` | `na_index-4x6_4x6in` | preselected size. Falls back to LPrint's `media-default` if LPrint doesn't list it |
| `MAX_UPLOAD_MB` | `20` | upload cap |
| `MAX_PAGES` | `50` | most PDF pages one print may send |
| `MAX_COPIES` | `100` | |
| `RENDER_DPI` | `203` | resolution of PDF labels and ZPL previews. Match the printer |
| `GOMEMLIMIT` | `400MiB` (set in the image) | Go's heap limit, under the example's 512 MB `mem_limit`. Raise the two together |
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

The page sends its box edits as the `crops` form field: JSON mapping a page number to its boxes, each
`[x, y, w, h]` in fractions of the page as displayed, from the top left. An optional fifth number, `1`, flips that
box's label 180° (`0`, or no fifth number, leaves it).

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
[ingridhq/zebrash](https://github.com/ingridhq/zebrash). The barcode check uses
[gozxing](https://github.com/makiuchi-d/gozxing) (MIT), a Go port of ZXing.

The labelprint binary and container images are based in part on the work of the FreeType Team: zebrash draws text
with [golang/freetype](https://github.com/golang/freetype), a Go port of [FreeType](https://www.freetype.org), used
under the FreeType License ([FTL](https://github.com/golang/freetype/blob/master/licenses/ftl.txt)).
