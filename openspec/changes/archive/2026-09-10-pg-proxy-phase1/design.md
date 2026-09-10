# Design

> Source spec: `docs/superpowers/specs/2026-09-09-pg-proxy-phase1-design.md`

This change implements the design in the linked spec. See that document for problem framing,
goals, architecture, and decisions. The sections below contain only opsx-specific framing
not covered in the source spec.

## Opsx-specific notes

Capability → source spec 章節對照，供實作時定位：

| Capability | Source spec 章節 |
|---|---|
| `pgwire-protocol` | §6.1 首包四型別、§6.2 StartupMessage、§6.3 ErrorResponse |
| `sni-routing` | §7 全章（annotation、dynamic informer、原子替換、生命週期、RBAC） |
| `connection-lifecycle` | §5 四個接縫、§6 11 步流程、§6.4 後端交涉、§9.1 shutdown、§9.3 上限、§9.4 idle |
| `config-hot-reload` | §8 全章（設定項、定期重讀、subPath 禁令） |
| `pgproxy-observability` | §9.4b probe、§9.5 metrics、§13 log 格式 |
| `pgproxy-deployment` | §3 架構、§9.4c PROXY protocol、§10.3–10.4 憑證與 DNS、§11 交付物、§12 已知限制 |
