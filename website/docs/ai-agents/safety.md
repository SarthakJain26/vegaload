---
title: Safety
description: How VegaLoad stops an agent from sending load at a host you did not approve. The allow-list, the audit log and the caps.
sidebar_position: 3
---

# Safety

A load test sends real traffic. If an agent can start one, you need to know where it can point. VegaLoad has three guardrails: a host allow-list, an audit log, and hard caps.

## The host allow-list

Localhost is always allowed. Any other host needs approval.

**From a terminal**, VegaLoad asks you:

```
Target "https://staging.example.com/health" is not localhost or allowlisted. Continue? [y/N]
```

To skip the question for a host you trust, add it on the command line:

```bash
vegaload run -target https://staging.example.com/health -protocol http1 \
  -allow-target staging.example.com
```

With no terminal, such as in CI, VegaLoad does not ask. It refuses:

```
vegaload run: refusing to run against "https://staging.example.com/health" without confirmation (pass -yes, or add it with -allow-target)
```

**From an agent**, only you can approve a host. `run_test` and `validate_scenario` have no `yes` or `allow_targets` argument, and refuse a call that has one. When the run reaches a host that is not localhost, one of two things happens:

- If your agent host can ask you a question (MCP elicitation), it asks: "Your agent asked VegaLoad to send load to host staging.example.com ... Allow it for this one run?" The question shows the command. Only if you say yes does VegaLoad run the same command again, with `-allow-target staging.example.com` added and nothing else. If you say no, nothing is sent to that host.
- If your agent host cannot ask, the tool returns an error with the exact command. The agent shows it to you, and you run it in your own terminal:

```
vegaload run -vus 5 -duration 30s -allow-target staging.example.com checkout.vl.js
```

Your approval lasts for that one run. The next run asks again. The MCP server over `GET /sse` can ask you. Over `POST /mcp` it cannot, so it returns the command.

The check applies to every call, not only to `target`. A scenario that calls a second host with `http`, `grpc`, `redis` or any other global is checked per call, and a run stops at the first call to a host that is not allowed. Some protocols check only the first host you name. For example, a Kafka run checks the first broker, not the others the cluster announces. Each protocol's notes say so.

## What the agent is told

The guide files from `vegaload init` tell the agent that you approve each new host, and to start short: 10 seconds and a few users.

You can still add your own controls:

- Keep Cursor's and Claude Code's approval step on for `run_test`. You then see each call, with its target, before it runs.
- Use `-allow-target` yourself, in a terminal, for the few hosts you trust.

## The audit log

Every run that starts is written to `~/.vegaload/audit.log`, one JSON line per run. This line is from a run started by an agent:

```json
{"time":"2026-10-13T10:55:50Z","trigger":"mcp","target":"http://127.0.0.1:18555/","protocol":"http1","executor":"fixed-vus","vus":2,"duration_ns":2000000000,"outcome":"success","total":2926,"thresholds":["p95 < 500ms"],"thresholds_passed":true}
```

`trigger` says who started the run: `mcp` for an agent, `cli` for the command line. The log stays on your machine.

## Hard caps

Every run has caps. The defaults are high. Lower them for your team.

| Option | Cap | Default |
| --- | --- | --- |
| `-cap-vus` | Most virtual users | 10000 |
| `-cap-duration` | Longest run | 24h |
| `-cap-rate` | Most iterations per second (constant-arrival-rate) | 100000 |

```bash
vegaload run -target http://127.0.0.1:8080/ -protocol http1 \
  -vus 50 -duration 1m -cap-vus 100 -cap-duration 5m
```

## Other safe defaults

- PostgreSQL and MySQL tests are read-only by default.
- FTP and SFTP runs write test files. Run them against a server that is meant for tests.
- The report is a local file. VegaLoad does not upload it.
- `diagnose_failure` adds a plain-English summary only if you set `VEGALOAD_LLM_PROVIDER`. If you do, the report data goes to the provider you chose.
