# CI merge gates

The `main` branch requires a pull request, resolved review conversations, and these
GitHub Actions checks before merge:

- `dart`: formatting, analysis, Flutter/example tests and publication validation.
- `wgc`: WGC adapter race tests and static checks.
- `build-tools`: native tooling tests and pinned build-plan validation.
- `OpenAI Code Review`, `OpenAI Issue Review`, `OpenAI PR Review`: the shared reviewer.
- `Native CI gate`: conditional native validation with an unconditional PR result.

The native workflow starts for every pull request. Its `changes` job determines
whether native inputs changed. If so, validation, both Android/Apple builds and
combined release-input verification must all succeed. If not, those jobs are
skipped and the gate reports that no native build was required. A failed change
comparison, missing decision, failed/cancelled build or unexpected job skip fails
the gate. This avoids leaving documentation-only PRs waiting for a path-filtered
workflow to report a required status.

Required checks are bound to the GitHub Actions application. The branch must be
up to date before merging. Protection applies to administrators, and force pushes
and branch deletion are disabled. No additional human approval count is required;
review checks and conversation resolution remain mandatory.

Repository auto-merge support allows a PR to be armed for squash merge once these
requirements pass. Enabling the repository setting does not arm all future PRs;
auto-merge is enabled separately for each requested PR. Auto-fix remains separate
and addresses CI/review failures without bypassing the gates.
