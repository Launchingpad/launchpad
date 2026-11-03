# Security policy

Launchpad is early software (see `README.md`). It is not ready for
production use by other people. Still, we want to hear about security
problems.

## Report a vulnerability

Report it privately. Do not open a public issue or pull request for a
security problem.

Use GitHub private vulnerability reporting: open the **Security** tab of
this repository and choose **Report a vulnerability**.

Please include:

- what the problem is and what an attacker can do with it;
- the steps, request or script that show it;
- the version (commit hash) you tested.

Never put a real secret, token or personal data in a report.

## What to expect

This is a small project with no support team. We read each report and answer
as soon as we can, but we give no fixed response time. We will tell you when
the problem is fixed. We credit reporters who want credit.

## Supported versions

There are no releases yet. Only the latest commit on the default branch is
supported.

## Notes for operators

- The first-run setup link is the one credential the server logs. Anyone who
  can read the app log before setup is done can take over the install.
- Keep `deploy/.env` private (mode 0600). It holds every secret of the
  stack.
- The `docker-compose.yml` in `server/` is for local development. It has
  public default passwords and keys. Outside `LOG_ENV` `local` or `test`, the
  server refuses the default S3 secret and the default settings encryption
  key. It does not check the Postgres password or the OTP pepper against
  those defaults, so set your own. Never expose that stack to a network.
