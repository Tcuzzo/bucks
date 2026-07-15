# Security Policy

BUCKS is real-money software. A security bug here is not a broken web page — it can
touch someone's broker account. Please treat it that way.

## Reporting a vulnerability

**Report privately via [GitHub Security Advisories](https://github.com/Tcuzzo/bucks/security/advisories/new).**

**Never file a public issue for an exploitable bug.** A public report of a live
exploit puts every BUCKS owner's money at risk before a fix exists. Use the private
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

- bypassing the paper/live gate or the per-session live confirmation
- placing orders outside the risk band or past the kill switch
- reading broker keys, the Telegram token, or AI keys out of the encrypted store
- tricking the Telegram approval flow (approving as someone else, replaying approvals)
- getting the updater to install an unapproved binary

Design discussions, feature requests, and non-exploitable bugs are normal public
issues — the private channel is for anything an attacker could use.
