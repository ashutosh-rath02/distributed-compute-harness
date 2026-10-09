# Security policy

## Reporting a vulnerability

Please report it privately; do not open a public issue.

- Use GitHub's "Report a vulnerability" button on the repository's
  Security tab (private advisory), or
- email the maintainer at ashutosh123rath@gmail.com with "home-harness
  security" in the subject.

Include what you found, how to reproduce it, the version (`manager -version`
or `GET /version`) and what an attacker gains. You will get an
acknowledgement within a few days. Please allow time for a fix before
disclosing; credit is given if you want it.

## What matters most here

The design assumes the manager never trusts agents, AI models or relays.
Reports that break these are the most valuable:

- running code or reading data on a device without the operator's token or
  an approved enrolment;
- an agent, model or relay escaping its role (forging another node's
  identity, reading another device's data, approving its own work);
- anything that exposes the operator token, pairing token or relay keys;
- script injection in the dashboard through agent- or model-supplied text;
- a way to make a node install an unsigned or unvetted binary.

Out of scope: attacks that need the operator's own account on the
manager's machine, and the plain-HTTP join page on a network the owner
chose to trust (documented in `HANDOVER.md` §11).

## Supported versions

Only the latest release and `main` get fixes.
