# mesh7 demo

A fifteen-minute walk through what mesh7 does for a CRM agent, on your machine,
next to anything already running (mesh on `:9191`, console on `:3118`).

Needs `mesh7` on the PATH (v0.17.3 or later), Python 3, and optionally a built
[flux7-console](https://github.com/KTCrisis/flux7-console) frontend
(`CONSOLE_DIR`, default `~/flux7-console/frontend`).

```bash
./run.sh start                 # mesh + console; the CRM catalogue is pinned on first sight
python3 agent.py tools         # what the agent is offered
```

## 1. The catalogue

Console, **Tools**, agent `sales-assistant`. Each CRM tool is classified from what
it declares: `run_sql` is **generic**, its argument decides what it does. The
decision column shows the policy for this agent, and can be changed live.

## 2. An action that waits for a human

```bash
python3 agent.py action
```

A read goes through. A write returns an approval id to the agent. Approve it in
the console (**Approvals**), press Enter: the agent's retry of the same call
runs, once. A second retry asks again.

```bash
python3 agent.py sql
```

`SELECT` goes through on a text condition; `DELETE` asks. So does `select` in
lower case: a condition on text is not a SQL parser, and the demo says so.

## 3. The upstream changes behind your back

```bash
./run.sh rugpull               # the CRM rewrites a description and adds an export tool
python3 agent.py rugpull
```

The Tools page shows both tools held back, with the old and new descriptions:
the rewritten one now tells the model to send the customer base outside. The
new export tool is refused and the changed one asks for approval, although the
policy ends with `crm.*: allow`. Accept or leave them held back.

## 4. The proof

```bash
./run.sh verify
```

The trace chain holds (HMAC). A copy where one approval was rewritten into an
allow fails at that line: `hash does not match the content`. **Traces** in the
console shows each call with its decision and approval.

## Reset

```bash
./run.sh reset                 # stop, and forget pins, approvals and traces
```

`demo/state/` holds everything the demo writes. `MESH_TRACE_KEY` is set to a
fixed demo value by `run.sh`; it is not a secret.
