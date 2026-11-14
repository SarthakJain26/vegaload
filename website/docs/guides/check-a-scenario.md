---
title: "Check a scenario before a real run"
description: "`validate` runs the scenario once, with one user and one iteration, and says whether it works."
sidebar_position: 13
---

# Check a scenario before a real run

```
vegaload validate examples/scenarios/crud-flow.vl.js
```

`validate` runs the scenario once, with one user and one iteration, and says
whether it works. It catches a script that will not load (a syntax error, a
missing file, no `iteration()` in Python) and an iteration that fails (a
wrong URL, a thrown error, a host that is not allowed). It makes real
network calls, under the same host rules as `run`, so a host that is not
localhost needs `-allow-target` or `-yes`. A refused host makes the scenario
not valid, even when the script catches the error. Any `check()` results are
listed, and a failed check does not make the scenario invalid.

When the error comes from a line of the scenario, `validate` names it, also
for TypeScript and Python:

```
not valid: throw.vl.js (iteration)
  js: Error: boom
  at throw.vl.js:4:9
```

Add `-output json` for a machine-readable result. Besides `valid`, `stage`
and `error`, it has `error_location` (`file`, `line`, `column`), `config`
(the settings the iteration ran with, and the names of the data files and
variables, never their values), `hosts` (the hosts the iteration reached) and
`refused_hosts` (the hosts to approve with `-allow-target`). The hosts are
those of this one iteration. See [Commands and flags](../reference/cli.md#validate)
for every field.

| Exit code | Meaning                          |
|-----------|----------------------------------|
| 0         | The scenario loaded and its iteration ran |
| 1         | The scenario is not valid        |
| 2         | Bad usage                        |

From an agent, the `validate_scenario` tool does the same and returns the
same JSON. When a host is refused, the user is asked to approve it.
