# Security Policy

## Reporting a vulnerability

Please **do not** open a public issue for security problems. Report privately via
GitHub's **"Report a vulnerability"** button on the repository's **Security** tab
(Security Advisories). Include steps to reproduce and the affected commit/version.

This is a research prototype maintained on a best-effort basis; expect an
acknowledgement within a few days.

## Supported versions

Fixes land on `main`. Pre-1.0 releases (`v0.x`) are not separately patched — please
test against `main`.

## Scope & security model

Prism loads eBPF and runs privileged (`prismd` is a privileged DaemonSet or needs
`CAP_BPF`/`SYS_ADMIN`), so deploy it like any privileged node agent.

The core safety property is **verifier-enforced**: the shared `prism_identity` map
is created `BPF_F_RDONLY_PROG`, so **`prismd` is the sole writer** and consumers can
only read it — a buggy or hostile consumer cannot corrupt anyone's identity. Details
are in [`spec/README.md`](spec/README.md) (§2.2, §5).

> **Not production-hardened.** Cluster-wide identity coherence is an open problem and
> ARM is unvalidated (see the README *Status*). Evaluate accordingly before relying
> on Prism in a security-sensitive setting.
