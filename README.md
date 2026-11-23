# BrewDat Exfil Governance

Monthly evidence collection portal. Admins define controls and map them to owners. Owners sign in and upload evidence per month. The portal emails owners in the first week of every month and sends reminders at an interval the admin chooses until evidence is in.

Built as one Go binary (web server, scheduler, mailer) with an embedded SQLite database. No Node, no nginx, no separate database server.

## Install or update (one command)

First install on a fresh RHEL 8 or 9, Rocky or Alma server:

```bash
curl -fsSL https://raw.githubusercontent.com/black0405/brewdat-exfil-governance/main/deploy.sh | sudo ADMIN_EMAIL=you@company.com bash
```

Every update after that pulls the latest commit, runs the tests, builds and restarts:

```bash
sudo /opt/brewdat/deploy.sh
```

The script installs packages and Go 1.27.4 (checksum verified), clones or updates the repo in `/opt/brewdat`, runs `go test`, builds `/usr/local/bin/brewdat`, creates the `brewdat` system user, writes a hardened systemd unit, applies SELinux labels, opens the port in firewalld, starts the service and checks `/healthz`. If the tests or the build fail, the running service is not touched. On first install it prints a temporary admin password, and you choose your own password at first sign in.

Private repository: use a deploy key or token in `REPO_URL`, for example `https://TOKEN@github.com/ORG/REPO.git` (only needed if the repo is ever made private).

## After installing

1. Open `http://SERVER:8080` and sign in with the printed admin email and temporary password.
2. Go to **Email and schedule**: enter the SMTP server and the portal address, then press **Send test email**.
3. Under **People**, add the people who own controls. Share each temporary password with its owner.
4. Under **Controls**, add the controls and tick their owners.

## Controls, monthly figures and review

* **Controls** carry the columns of the Egress Point Monitoring sheet. Admins can import the workbook under **Controls > Import from Excel**. Rows are matched on Sr No, which becomes the control number (1 becomes B01), owners are matched by email or full name, and merged cells are filled down. Re-importing updates existing controls.
* **Monthly figures:**
  * **Identified** is entered once, in the control's first month, and becomes a fixed baseline that owners cannot change. Admins can correct it on the control.
  * **Remediated** is the running total, entered each month.
  * **Accepted risk** is set by admins.
  * **Pending** is calculated as Identified − Remediated − Accepted risk.
  * **Compliance score** is Remediated ÷ Identified.
* **No change from last month** resubmits the previous figures without new files.
* **Review:** every submission goes to **Review** for an admin to approve or deny, and a denial needs a reason. Owners are emailed either way. A control counts as done for the month only once approved. Reminders pause while evidence waits for review and restart after a denial.
* **Overview** shows every control as a card with Identified, Remediated, Pending and Compliance score.
* **Executive view** (admins) shows:
  * a speedometer for overall compliance across all controls, with key totals beside it;
  * controls ranked by compliance, lowest first, grouped by Category and listed one by one, each labelled At risk (below 70%), Watch (70% to 90%) or On track.
* **Who submits:** only a control's owners submit evidence. Admins view and review.
* **Monthly sheet** shows every column and figure for any month. **Download Excel** gives one sheet per month in a single workbook.

## Email templates and the yearly baseline

* **Templates:** under **Email and schedule > Templates**, admins edit the monthly request, January baseline request, reminder, approved and denied emails. Placeholders such as `{name}`, `{month}`, `{controls}` and `{link}` are filled in for each owner. **Reset to default** restores the original text.
* **Custom templates:** admins can add their own, for example an audit deadline notice, and send it to every owner or only to owners with open controls.
* **Yearly baseline:** Identified is a yearly baseline. The first submission each year (normally January) sets it, and the January request uses the baseline template to ask for it. Past months keep the baseline they were submitted under, so history and old scores do not change.

## How emails work

* **Monthly request:** sent on the chosen day (1 to 7) from 08:00 server time. Each owner gets one email listing their controls that have no evidence for the month yet.
* **Reminders:** repeat every N days (admin setting, 0 turns them off) until every control the owner holds has evidence for the month.
* **No duplicates:** every send is recorded in the database, so restarts never cause a second email. If the server was down on the request day, the request goes out as soon as it is back.
* **Shared controls:** one owner uploading satisfies the control for all of its owners.
* **Time zone:** the schedule uses the server's time zone. Set `TZ=` in `/etc/brewdat/brewdat.env` to override it.

## Server requirements

| | Recommended for 50 to 500 users | Minimum |
|---|---|---|
| CPU | 2 vCPU | 1 vCPU |
| RAM | 2 GB | 1 GB plus 2 GB swap |
| OS disk | 20 GB | 20 GB |
| Data disk at `/var/lib/brewdat` | 50 GB, growable LVM | Sized to the evidence volume |
| OS | RHEL, Rocky or Alma 9 (8 works), x86_64 or aarch64, SELinux enforcing | |

* The running portal uses about 5 to 80 MB of RAM and almost no CPU. The 2 GB is for compiling on the server during deploys; later builds are cached and take seconds.
* **Storage estimate:** controls × 12 months × average evidence size. For example, 300 controls × 12 × 5 MB is about 18 GB a year.

### Other things you need

* **SMTP relay:** host and port (587 with STARTTLS is the usual choice), credentials or an IP allowlist for the server, and a sender address the relay may send as. Add SPF and DKIM for that domain so mail is not marked as spam.
* **DNS name**, for example `evidence.company.com`, and a **TLS certificate** for it.
* **Outbound access during deploys** to `github.com`, `go.dev`, `dl.google.com`, `proxy.golang.org` and `sum.golang.org`. If outbound access is blocked, commit a `vendor/` folder and build with `-mod=vendor`.
* **Time sync:** keep `chronyd` running so the schedule fires on the right day.
* **Off server backups** of `/var/lib/brewdat` and `/etc/brewdat`, by VM snapshot or rsync.

## HTTPS

Choose one of these:

* **The portal serves HTTPS itself:** put the certificate and key in `/etc/brewdat/tls/` (owner root, group brewdat, mode 0640). Then set the following in `/etc/brewdat/brewdat.env`:

  ```
  BREWDAT_ADDR=:443
  BREWDAT_TLS_CERT=/etc/brewdat/tls/cert.pem
  BREWDAT_TLS_KEY=/etc/brewdat/tls/key.pem
  BREWDAT_BASE_URL=https://evidence.company.com
  ```

  Then run `sudo /opt/brewdat/deploy.sh` to open the new port and restart. The service already has the capability to bind port 443. For Let's Encrypt, use `certbot certonly --standalone` with a deploy hook that copies the files and runs `systemctl restart brewdat`.
* **A company load balancer or proxy terminates HTTPS:** set `BREWDAT_BEHIND_PROXY=1` so cookies are marked secure and client IPs come from `X-Forwarded-For`.

Plain HTTP on port 8080 is only for first boot checks, because passwords would travel unencrypted.

## Settings file `/etc/brewdat/brewdat.env`

| Variable | Default | Meaning |
|---|---|---|
| `BREWDAT_ADDR` | `:8080` | Listen address |
| `BREWDAT_DATA` | `/var/lib/brewdat` | Database, evidence files, daily backups |
| `BREWDAT_BASE_URL` | | Portal address used in emails; it can also be set in the UI |
| `BREWDAT_TLS_CERT`, `BREWDAT_TLS_KEY` | | Serve HTTPS directly |
| `BREWDAT_BEHIND_PROXY` | `0` | Set to `1` behind an HTTPS proxy |
| `BREWDAT_MAX_UPLOAD_MB` | `25` | Size limit per file (10 files per upload) |
| `TZ` | server zone | Time zone for the email schedule |

SMTP settings and the schedule are stored in the database and edited in the UI. The SMTP password is stored in plain text in the database, which only the `brewdat` user can read.

## Operations

```bash
sudo systemctl status brewdat          # service state
sudo journalctl -u brewdat -f          # live log, including every email sent
sudo /opt/brewdat/deploy.sh            # update to the latest commit

# Locked out? Reset (or create) an admin and print a new temporary password:
sudo runuser -u brewdat -- env BREWDAT_DATA=/var/lib/brewdat /usr/local/bin/brewdat admin you@company.com
```

**Backups:** a consistent copy of the database is written to `/var/lib/brewdat/backup/` every day, and the newest 7 are kept. Evidence files live in `/var/lib/brewdat/files/`. To restore:

1. Stop the service.
2. Copy a backup over `/var/lib/brewdat/brewdat.db`.
3. Delete `brewdat.db-wal` and `brewdat.db-shm`.
4. Start the service.

## Security

* **Passwords and sessions:** passwords are hashed with bcrypt (cost 12). Session tokens are random and stored hashed. Cookies are HttpOnly and SameSite.
* **Forms and sign in:** every form has a CSRF token. Sign in is rate limited per IP.
* **Browser headers:** a strict Content Security Policy is sent, with no inline scripts.
* **Uploads:** only allowed file types are accepted, and size is limited. Files are stored under random names, so user file names never touch the filesystem. Downloads are always served as attachments.
* **Access:** users only see controls they own, and everything else returns not found.
* **First admin:** there are no default credentials. The first admin is created by the deploy script with a one time password.
* **Service hardening:** the systemd unit runs as an unprivileged user with a read only system, private tmp and a syscall filter.

## Development

```bash
go test ./...
go run .                                # http://localhost:8080, data in ./data
go run . admin you@example.com          # create a local admin
```

Visible UI text uses no hyphen or dash characters (dates read "23 Nov 2026", periods read "November 2026"). A test fails if a page or email shows one. Text that people type, such as file names or emails, is shown as entered.

## Not built yet

These can be added when needed: a self service forgot password email, an audit log, a "send reminders now" button, skipping weekends, and periods other than monthly.
