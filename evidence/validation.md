# Validation evidence — 2026-10-04

The five CLI integration scenarios passed: repair + resume (14 tasks), direct
approval (10 tasks), cancellation (expected failure), mock compatible provider
(three API calls, 10 tasks) and malformed provider output (expected failure).
Go race tests and go vet passed. Playwright verified the desktop dashboard,
checkpoint/failure/fresh-review details, full replay and mobile layout without
browser errors or horizontal overflow. The timeline video is about 24 seconds.

The included report is run `cf-ea453072`. Its mock local-process Create→Ready
median is 16 ms; range 0–36 ms. Millisecond
rounding can report 0 for very short intervals. These are measured local process
figures, not an AX/Substrate performance result. Fixture workers made no LLM calls.

Real AX/Substrate execution, real Unsloth inference, Docker/Compose builds,
GitHub-hosted CI and MR/deployment integration were not executed in this environment.

Reproduce core checks with `make verify`; reproduce visual checks with
`npm ci`, `npx playwright install chromium` and `npm run capture -- PATH/replay.html`.
