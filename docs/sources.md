# Official API references verified 2026-10-04

| Source | What the implementation uses |
| --- | --- |
| [AX ax.proto](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/pkg/apis/v1alpha1/ax.proto) | generated typed AX gRPC client/server; `CreateTask`, `UpdateWorkspace`, `GetTask`, `SuspendTask`, `ResumeTask`, `DeleteTask`, readiness conditions |
| [AX runner contract](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/docs/runner.md) | fixed command-runner entrypoint, durable workspace, fresh process tree, readiness endpoints, keep runner after command exit |
| [AX server](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/internal/server/server.go) | immutable tasks; resume of an existing identity |
| [AX workspace setup](https://github.com/google/ax/blob/ac2332829f22360ff97b0ba34d94dd0dd782f17e/internal/workspace/setup.go) | default marker location and workspace rewrite/fetch behavior |
| [ADK workflow](https://github.com/google/adk-go/tree/8c7ab1baf9bcd9eedd28e4b13ed71f37c239b41d/workflow) | FunctionNode, JoinNode, StringRoute, NodeConfig timeouts/retries, WithMaxConcurrency |
| [ADK Runner](https://github.com/google/adk-go/blob/8c7ab1baf9bcd9eedd28e4b13ed71f37c239b41d/runner/runner.go) | NewInMemory and graph-driving custom Agent |

No AX-supported LLM provider is involved. The coding CLI calls `/chat/completions`
directly. Agent-to-agent exchange uses the application's authenticated artifact
broker, not the A2A protocol; the runtime boundary remains the actual AX gRPC API.
