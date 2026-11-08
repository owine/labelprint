# Security policy

## What labelprint is, security-wise

labelprint turns an HTTP upload into a print job. It has **no login of its own**: anyone who can reach it can print,
cancel jobs still in LPrint's queue, see LPrint's recent job names, and read the printer's status. Run it behind something that authenticates, such as
an auth proxy (Authelia, oauth2-proxy, …) in front of a reverse proxy, or a network only you can reach. Never expose
it to the internet directly.

The uploads it handles are shipping labels, which carry names and addresses. Its design keeps them local:

- **No outside services.** PDFs are rendered with poppler and ZPL with zebrash, both inside the container. The page
  loads nothing third-party; the Content-Security-Policy is `default-src 'self'`.
- **Nothing kept.** Uploads are held in memory for the request. A PDF is written to `/tmp` (a tmpfs in the example
  compose) only while it renders, then deleted. A file shared from a phone is held in memory under a random
  one-time link until the page collects it, for 10 minutes at most. Logs record the file type, size, copies, media
  and job ids, not file names, share links or contents.
- **Cross-site requests are refused.** An auth proxy's session cookie goes with every request to the host, so
  labelprint rejects state-changing requests from other sites (Go's `http.CrossOriginProtection`).
- **Hardened container.** It runs as uid 65532, works with a read-only root filesystem and all capabilities dropped,
  and ships no shell tools beyond Alpine's base.
- **Untrusted parsers are contained.** The ZPL renderer runs under panic recovery and a timeout; poppler runs as a
  subprocess with a timeout. Upload size, page count and copies are capped.
- **Job user names are opt-in.** `USER_HEADER` names a request header (e.g. `Remote-User`) whose value labels jobs
  in LPrint. Only set it if your proxy always overwrites that header, or a client can choose the name that's shown.
  It's a label, never an access decision.

Every image is built in CI, smoke-tested, and blocked from publishing if Trivy finds a fixable HIGH or CRITICAL
vulnerability. Renovate keeps the base image, apk packages, Go modules and Actions pinned and current.

## Supported versions

Only the latest release (and `main`) gets fixes.

## Reporting a vulnerability

Please report privately through GitHub Security Advisories:
https://github.com/owine/labelprint/security/advisories/new

Include what you found, how to reproduce it, and the version (image tag or digest). This is a personal project, so
replies are best-effort, but security reports come first.
