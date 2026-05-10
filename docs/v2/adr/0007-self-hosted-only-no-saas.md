# ADR 0007: Self-hosted only; no SaaS

**Status**: Accepted
**Date**: 2026-05-10
**Decision-makers**: phall
**Related**: [ADR-0005](0005-multi-user-data-model-from-day-one.md),
[ADR-0010](0010-one-daemon-many-users.md)

## Context

A reservation-sniper that "just works" for strangers via a hosted
service is technically possible and commercially attractive. It is
also a meaningfully different product, with meaningfully different
risks:

- **Resy ToS.** Automated booking is almost certainly disallowed.
  Doing it for yourself in your own home is one risk profile;
  charging strangers for it is another. Resy has both motive (lost
  inventory) and a clear target (a public domain) to play whack-a-mole
  with hosted services. Self-hosted is a distributed problem; SaaS is
  a centralized one.
- **Secrets.** SaaS means storing other people's Resy passwords,
  session JWTs, and (for some venues) credit cards. That's PCI-adjacent
  and a real liability if the service is breached.
- **Abuse.** Public services attract abuse vectors a private homelab
  doesn't (account stuffing, scraping, fake users).
- **Build cost.** Multi-tenant SaaS is ~quarters of work and a runbook
  before it's safe to expose. Self-hosted is ~weeks.

## Decision

resy-snipe is **self-hosted only**. We ship a binary, a Docker image,
a systemd unit, and operator docs. We do not run a hosted instance
for anyone but ourselves. We do not collect telemetry. We do not
phone home.

The deployment shape we design for is one homelab box per operator,
serving that operator and a small number of friends-and-family
([ADR-0010](0010-one-daemon-many-users.md)).

## Consequences

**Positive**
- Trust boundary is the box. The operator is the friends' "service
  provider" — they vouch for the box, the operator owes them backups
  and uptime, no third party is in the loop.
- Each instance is independent. A breach of phall's box doesn't
  affect james's box. No central abuse target.
- We can be opinionated about secrets handling
  ([ADR-0008](0008-secrets-sealed-at-rest-operator-key.md)) without
  worrying about onboarding strangers.
- No legal exposure beyond what already exists for using Resy via
  scripts.

**Negative**
- No "I just want to try it without setting up a server." The path
  to use is `git clone` → `docker compose up`. We accept this as the
  cost of staying defensible.
- Each operator independently maintains their box. No central upgrade
  rollout; ship release notes and trust operators to `docker pull`.

**Neutral**
- The architecture would technically support SaaS (it's just multi-user
  with a public ingress). We deliberately don't enable that path —
  all design decisions favor "small known userbase" over "many unknown
  users."

## Alternatives considered

1. **Free SaaS on a single Cloudflare-fronted box.** *Rejected:* the
   ToS and abuse exposure isn't worth the convenience. As soon as it
   gets known, Resy blocks the IP range and abuse pours in.
2. **Paid SaaS.** *Rejected:* would require legal review, terms,
   refund policy, customer support, payment processing. Out of scope
   for a friends-and-family tool.
3. **Self-hosted + an "official" hosted instance for our friend group.**
   *Rejected:* that's just SaaS with a smaller marketing budget. The
   risks are the same.

## Notes

If the value-prop case for hosted ever changes (e.g. Resy launches an
official partner program, or someone wants to fork this and run it
commercially under their own name and risk), this ADR gets superseded.
Until then: the answer is "run it on your box, or run it on someone's
box you trust."

A practical implication: the README and getting-started doc never
direct users to a URL or signup form. They direct users to a `docker
run` command, period.
