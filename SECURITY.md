# Security Policy

BUCKS is paper-only trading software, but it stores broker credentials and contains
order-routing code. A security bug can still expose an account. Please treat it seriously.

## Reporting a vulnerability

**Report privately via [GitHub Security Advisories](https://github.com/Tcuzzo/bucks/security/advisories/new).**

**Never file a public issue for an exploitable bug.** A public report of an active
exploit can expose BUCKS owners' credentials before a fix exists. Use the private
advisory; you'll get a response there, and credit in the fix if you want it.

What helps: the version you ran (`bucks version` prints it), your platform, and the
smallest reproduction you can manage. What to skip: proof-of-concept trades against a
real account — paper mode reproduces everything the trading path does.

## Supported versions

The **latest release** gets security fixes. Older releases do not — BUCKS ships as a
single static binary and updating is one download, so there is no reason to run an
old one.

## What counts

Anything that could move money, leak keys, or silence a safety rail, including:

- bypassing the refusal of real-money brokers
- placing orders outside the risk band or past the kill switch
- reading broker keys, the Telegram token, or AI keys out of the encrypted store
- tricking the Telegram approval flow (approving as someone else, replaying approvals)
- getting the updater to install an unapproved binary

Design discussions, feature requests, and non-exploitable bugs are normal public
issues — the private channel is for anything an attacker could use.
