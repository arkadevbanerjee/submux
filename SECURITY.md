# Security policy

submux sits between Claude Code and your model backends and forwards your own
OAuth tokens and API keys. Bugs that leak, log or misroute credentials are in
scope, as are request smuggling, config parsing and anything reachable from the
listening port.

## Reporting a vulnerability

Please do not open a public issue. Report privately through
[GitHub private vulnerability reporting](https://github.com/arkadevbanerjee/submux/security/advisories/new).

You can expect an acknowledgement within 7 days. Fixes ship on `main` and in the
next tagged release, with credit in the advisory unless you ask otherwise.

## Supported versions

Only the latest release and `main` receive fixes.
