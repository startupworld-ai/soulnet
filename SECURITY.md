# Security Policy

soulnet carries a cryptographic wire protocol (Ed25519 signing, X25519 key agreement, AES-GCM envelopes) and a settlement ledger. Please report vulnerabilities **privately** rather than in public issues.

- Email: security@startupworld.cn (or a private GitHub Security Advisory / 或 GitHub 私密 Security Advisory)
- Please include: affected package / version, reproduction, impact. We aim to acknowledge within 72 hours.
- Do not test against the public relay `relay.soulnet.startupworld.cn` with traffic that could affect other users; run your own relay (`cmd/soulnet-relay`) for research.
