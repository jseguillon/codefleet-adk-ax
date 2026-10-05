# AX suspend/resume: executable contract and limits

Verified against AX commit `ac2332829f22360ff97b0ba34d94dd0dd782f17e`.

## The process does not continue at its instruction pointer

The official runner contract says suspend snapshots the durable `/workspace`
directory, and resume restores it in a fresh container with a new process tree.
AX starts `/usr/local/bin/ax-task-runner` again; that runner launches the same
`spec.command` again. The application must reconstruct its own state from files.

| Item | Survives the documented resume boundary? | Demo handling |
| --- | --- | --- |
| Files under `/workspace` | yes, subject to successful substrate snapshot | checkpoints, code and initialization markers live here |
| Running process / RAM / call stack | no | command starts again and reloads its checkpoint |
| An HTTP/LLM request in flight | no continuity guarantee | may repeat if output was not checkpointed |
| Files elsewhere in the container | not promised by the runner contract | not used for recovery |
| Same task identity and immutable command spec | yes | `ResumeTask(atespace, name)` |
| New prompt/command passed to ResumeTask | no field for it | create another task, or design a separate command input channel |
| Published external side effect | survives externally, independently | requires reconciliation and idempotency, not blind replay |
| Orchestrator RAM / broker registrations | outside AX task snapshot | broker stays alive in Part 1; its crash recovery is not implemented |

The source has `CreateTask`, not an `UpdateTask` operation. Creating over an
existing task returns `FailedPrecondition` because the task is immutable.
`ResumeTaskRequest` only contains `atespace` and `name`. Editing a local YAML file
then resuming an existing task does not change its command or supply another prompt.
Workspace resources are updateable, but changing them must not be used to rewrite
an already prepared task or as an implicit task-input protocol.

## Actual demo restart protocol

1. Runner prepares each repository and its skill once. It atomically records
   `.codefleet-initialized.json` under that workspace on the durable volume.
2. CLI worker downloads its immutable, task-scoped input from the authenticated
   broker endpoint. It applies dependency artifacts, verifying SHA256 and base hash.
3. Worker atomically persists `checkpoint.json`, including the prepared baseline
   for its editable files. No model credentials are written to this file.
4. Worker emits a checkpoint event and enters the configurable presentation delay.
5. Orchestrator calls `SuspendTask`, waits until AX reports `Suspended`, then calls
   `ResumeTask` and waits for `Running` plus `WorkspaceReady=True`.
6. New worker loads the same checkpoint and skips input preparation. Its event
   includes a different PID and `reused_checkpoint=true`.
7. Worker generates/applies code, saves its result in the checkpoint, then publishes
   the immutable result to the broker. The graph consumes application results;
   an AX phase of `Running` does not mean the command succeeded.

The mock stops the old command process group, preserves its workspace directory
and launches another process. It does not emulate a real substrate volume snapshot,
VM checkpoint performance, or loss of the orchestrator. Those need a real AX run.

## Why this runner is custom

The pinned default AX runner stores maiden-run markers under `/ax`. The documented
durable mount is `/workspace`. Its workspace initialization writes inline files
and fetches/checks out Git sources when a marker is absent. We do not assume `/ax`
survives a resume: this project's markers reside in each durable workspace instead.
The regression test changes the file and workspace spec between preparations and
proves that a restored workspace is not rewritten.

The runner supports the needed command-only subset: ready/health endpoints,
metadata endpoints, declared files/one Git repo per workspace, persistent markers,
fixed toolchains, child process groups and bounded shutdown. It rejects bootstrap
goals and `debug=true`. It does not implement guest services, arbitrary skill
registries, MCP discovery or provider bootstrap.

## What is not exactly-once

An interruption after the model returned but before its output was checkpointed
can repeat generation. An interruption between file replacement and result saving
can re-enter generation against the saved prepared baseline. Thus computation is
**at least once**, while published job results are immutable.

The tiny fixture writes are deterministic, and the integration protocol rejects
stale artifact bases. This is sufficient for the demonstrated before-generation
suspend boundary, not a universal transactional agent protocol. The result HTTP
response could be lost after acceptance; a recovered caller must reconcile the
existing result rather than infer that nothing happened.

Do not put `git push`, MR creation, merge or deployment inside a blindly replayed
worker stage. Part 2 needs explicit effect identities, observed external state,
commit/digest checks and reconciliation before retrying any such operation.

For new orders inside a persistent command, build a queue consumer and checkpoint
its message position/application state. That is application logic; AX resume does
not add queue semantics, supply a new stdin payload, or keep an open subscription
alive across a process restart. A new task is the simpler choice when the repo,
skill, command or access scope changes.

Official source links: [runner contract](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/docs/runner.md),
[RPC and request fields](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/pkg/apis/v1alpha1/ax.proto),
[server immutability](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/internal/server/server.go),
[default workspace initialization](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/internal/workspace/setup.go).
