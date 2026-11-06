# Run Launchpad on CapRover

One one-click template installs everything. There is no second build step.

| App (name `launchpad`) | What it is | Public domain |
|---|---|---|
| `launchpad` | web app, sends `/api` to the API | `WEB_HOST` |
| `launchpad-api` | the API | none |
| `launchpad-files` | silo, the S3 API for attachments | `FILES_HOST` |
| `launchpad-db` | Postgres 16 | none |

The web app and the API share ONE host. The session cookie
(`__Host-odt_session`) belongs to one host only. Attachments need their own
host, because browsers use presigned URLs.

## Server size

Use a server with at least **2 GB of RAM**. CapRover itself uses about
400 MB, and Postgres, silo, the API and the web app share the rest. A server
with 1 GB of RAM runs out of memory when it loads a large move file (a 254 MB
file stopped the API with no error in the log). Use 4 GB if you plan to
restore a big install.

## Install

1. Create two DNS records that point at your CapRover server: `WEB_HOST` and
   `FILES_HOST` (for example `launchpad.example.com` and
   `launchpad-files.example.com`).
2. In CapRover open Apps, then One-Click Apps/Databases. Scroll to the
   bottom to the "TEMPLATE" box. Paste the full content of
   `deploy/caprover/one-click-app.yml` there and click Next.
3. Fill the form.
   - `Launchpad version` is a release such as `0.2.1`. The images
     `ghcr.io/launchingpad/launchpad-api` and `launchpad-web` must have that
     tag. There is no `latest`. The form also accepts `develop` and
     `develop-<7 hex characters>` (see "Track develop").
   - The template makes the random secrets for you, at the right length (the
     two encryption keys are 64 hex characters).
   - Keep the settings and backup keys. **Never change them** after the first
     start.
4. Click Deploy.
5. Add the domains. Open the app `launchpad` and add the domain `WEB_HOST`.
   Open the app `launchpad-files` and add the domain `FILES_HOST`. On both:
   enable HTTPS and tick "Force HTTPS".

## First run

Open the logs of the app `launchpad-api`. One line reads
`setup: open https://<WEB_HOST>/setup#<token>`. Open that link in your
browser. It is valid for 24 hours and works once. Do not share it.

A new link: restart the app `launchpad-api`.

Email sign-in needs mail. Until you set up mail, the sign-in page says "login
is not available on this server". The setup link needs no mail and logs you in.
Then open Settings, Platform, and set up mail (or fill in the two Postmark
fields in the template form).

To bring an existing install, use the "Restore" option on the setup page
instead (`docs/self-hosting.md`, move an install). Do not start a second
restore while the first one runs.

## Big uploads

A restore of a move file, and large attachments, pass through CapRover's own
nginx before they reach the web app. If an upload stops with "the result is
unknown", or the browser shows a 413 or a 504, look at CapRover's nginx
limits (request body size and timeouts) under Cluster, NGINX Configurations.
The web app's own nginx allows an unlimited restore upload on that one route.

## How the web app gets its config

The web app uses the stock web image. At start, its command writes the
production nginx config over the image's default one. That config comes from
`deploy/nginx/default.conf.template`, and the template file
`deploy/caprover/one-click-app.yml` holds a copy of it. Do not edit the copy.
Edit `deploy/caprover/one-click-app.yml.in` or the nginx file, then run
`deploy/caprover/gen-one-click.sh`. The test `deploy/caprover/test-one-click.sh`
fails when the generated file is out of date.

## Track develop

`develop` is a test channel, not a release. Use it only to try new work.
It can break, and a data migration in it may not be safe to undo.

The images `ghcr.io/launchingpad/launchpad-api` and `launchpad-web` get two
tags on each push to a `develop` branch of the public GitHub repository
(workflow `develop-images.yml`): `develop` (moves with each push) and
`develop-<short sha>` (fixed). There is no `latest`.

**The public repository has a `develop` branch only if the operator creates
and pushes one.** `scripts/export-public.sh` makes `master` only. Until a
`develop` branch is pushed to GitHub, the workflow does not run and no
develop image exists.

1. **Take a backup first.** The API runs the database migrations when it
   starts. There is no rollback: a newer schema stays after you go back to
   an older image. Back up the `launchpad-db` volume, or run a backup to
   remote storage in the app (Platform, Backups).
2. **Install with `develop`.** Type `develop` in the `Launchpad version` field
   of the template. To pin one build, type `develop-` and the short sha
   (the tag on GHCR, for example `develop-a1ac4ad`).
3. **Deploy by image name, to move an existing install.** In CapRover open the
   app `launchpad-api`, then Deployment, "Method 3: Deploy via ImageName".
   Enter `ghcr.io/launchingpad/launchpad-api:develop` and deploy. Do the same
   for the web app `launchpad`, with `ghcr.io/launchingpad/launchpad-web:develop`.
   The web app is built from the template's Dockerfile lines (its command
   writes the nginx config at start). Deploying a bare image replaces those
   lines and the command, so use "Method 4: Deploy via Dockerfile" for the web
   app and paste the template's lines (`FROM ...launchpad-web:develop` and the
   `CMD`) instead. When you are not sure, change the `Launchpad version`
   by creating a new app from the template.
4. **Redeploy after a push.** A tag that moves is not pulled again by
   itself. Redeploy by hand: in each app open the Deployment tab and use
   "Deploy via ImageName" again with the same image name, or run
   `caprover deploy -i <image>` from the CLI. Neither way is tested.
   CapRover also has a Deployment Webhook, but it may be tied to the
   git-repository method and not re-pull an image-name deploy. That is
   unverified: check the webhook route on a live install before you rely on it.
5. **Both apps on the same commit.** The API and the web app must run the same
   build. Redeploy `launchpad-api` and `launchpad` together, after the
   `develop-images` run has pushed BOTH images. For a fixed pair, use the same
   `develop-<short sha>` for both.
6. **Never change the settings key or the backup key.** A redeploy keeps the
   template's values. Do not edit `SETTINGS_ENCRYPTION_KEY` or
   `BACKUP_ENCRYPTION_KEY`, and do not make a new app with new keys on the
   old database.

This procedure is NOT tested on a live CapRover install. The workflow, the
form check and the tag names are checked; the redeploy steps are not.

## Update

The steps for an update are not tested yet. The API app and the web app must
run the same Launchpad version, and the two encryption keys must stay the
same. Read the release notes before you update. This section will get the
tested steps after the first update on a live install.

## Limits

- There is no nightly backup job in this template (CapRover has no
  scheduler). Back up the `launchpad-db` volume, or set up backups to remote
  storage in the app (Platform, Backups).
- Mail is optional. Set the Postmark values in the template form, or later
  in the app.
- Postgres and silo data live in Docker named volumes on one CapRover node.
- This template has been tested piece by piece (nginx config, silo start
  script, API config check). It has had little testing on a full CapRover
  install.
