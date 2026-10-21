# Starter Agent Catalog

`cr` ships no reviewers of its own: every review criterion comes from agent
definitions you point it at. This directory is a ready-to-use set of general
reviewers, so a new profile can review pull requests on day one and a team has
a base to extend instead of a blank page.

Each agent is a directory with an `index.yaml` (routing, model tier, effort)
and a `prompt.md` (what to review). The format is described in
[docs/review-guidance.md](../docs/review-guidance.md).

## What is included

| Agent | Runs | What it reviews |
| --- | --- | --- |
| `general:correctness` | Every PR that changes a non-Markdown file | Bugs the change introduces, each shown with a concrete input or sequence |
| `general:repo-conventions` | Every PR that changes a non-Markdown file | Rules the repository writes down in `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING`, `ARCHITECTURE`, and `docs/`, cited by document and section |
| `documentation:docs` | Every PR that changes a Markdown file | Accuracy of edited docs, including statements the same PR made false |
| `tests:behavior` | Selected for behavior changes | Whether tests would fail if the implementation were wrong |
| `security:trust-boundaries` | Selected for security-relevant changes | Exploitable vulnerabilities, classified by input trust and traced to a sink |
| `architecture:design` | Selected for structural changes | SOLID and dependency hygiene in the language's own idiom, one cited rule per finding |
| `structure:repo-health` | Selected for cross-cutting changes | Drift that compounds: undocumented decisions, crossed boundaries, unenforced rules |
| `automation:ci-release` | Selected for CI, build, and release changes | Gates, permissions, pinning, version identity, and failure visibility |
| `go:implementation-tests` | Selected for Go changes | Testable seams, context, error wrapping, typed boundaries, and tests that prove behavior |
| `rust:implementation-tests` | Selected for Rust changes | Soundness, reachable panics, async and locking, and tests that prove behavior |
| `tauri:config-ipc` | Selected for Tauri changes | Capabilities, IPC input validation, CSP, and updater configuration |
| `macos:swift-appkit` | Selected for Swift and AppKit changes | Main-thread use, leaks, crashes, entitlements, and Info.plist |
| `frontend:correctness` | Selected for browser frontend changes | Races, cleanup, browser security, loading and error states, and feature-flag fallbacks |
| `frontend:accessibility` | Selected for rendered UI changes | Accessibility barriers mapped to WCAG 2.2 success criteria |

The first three set `required_on_match: true`, so `cr` adds them whenever
their globs match a changed file, even if the selector leaves them out. The
rest are optional: the selector picks them by file globs and `applies_when`,
up to five optional reviewers per run by default. `architecture:design` uses
the `large` tier; everything else uses `medium`.

## Use it as a profile agent source

`cr` reads agent sources from local directories, live, at review time. Keep a
clone of this repository on `main` and point a profile at this directory:

```bash
git clone https://github.com/open-cli-collective/codereview-cli.git ~/src/codereview-cli
cr --profile work config agent-source add ~/src/codereview-cli/starter-agents
cr --profile work agents list
```

Pull the clone to pick up catalog changes. Do not point a profile at a working
copy you edit for other branches: a profile source applies to every review that
profile runs.

To try the catalog without changing a profile, pass it for one dry run:

```bash
cr review --dry-run --agents-dir ~/src/codereview-cli/starter-agents <pr-url>
```

`--agents-dir` adds to the profile's sources rather than replacing them, so
for a clean comparison use a profile that has no other agent sources.

## Make it your own

The catalog is a starting point. Two ways to extend it, which combine:

**A shared catalog for your organization.** Copy this directory into a
repository your team owns, edit the prompts, and add agents for your stack and
your organization-wide rules. Point every profile at that copy instead of this
one. An agent ID is `<category>:<name>`, from the directory names.

**Repository-local agents.** A repository can carry its own agents under
`.codereview/agents/`, in the same layout. `cr` reads them from the pull
request's base branch, so a PR cannot change the reviewers that review it.
Every repository-local agent that applies runs, without counting against the
optional limit. A repository-local agent with the same ID as a profile agent
replaces it for that repository: this repository, for example, replaces
`documentation:docs`, `structure:repo-health`, `automation:ci-release`, and
`go:implementation-tests` with versions that know its layout, and adds
`policies:conventions` for the shared standards it follows. See
[`.codereview/agents/`](../.codereview/agents/).

## Turn your AGENTS.md into a reviewer

`general:repo-conventions` reads a repository's guidance on every PR, but it
has to find and interpret the rules each time. For rules that matter, a
repository-local conventions agent that names them works better: reviewers
cite the rule, and coding agents that skipped the docs get caught at review.

1. Write the rule where people and agents read it first: `AGENTS.md`, a nested
   `AGENTS.md` for one subtree, or a topic guide under `docs/` that the entry
   file links to.
2. In the same PR, add the rule to a repository-local agent prompt, in a
   section whose heading names the document it comes from. When the rule
   changes later, change both in one PR.
3. Say what to flag and what not to flag, name the exceptions (or the
   exemption file that lists them), and say what the fix looks like.

A minimal agent at `.codereview/agents/repo/conventions/`:

```yaml
# index.yaml
name: conventions
description: Checks changes against the conventions in AGENTS.md and docs/.
model_tier: medium
effort: medium
file_globs:
  - "src/**"
  - "package.json"
applies_when:
  - A change touches application code or root tooling configuration.
```

```markdown
<!-- prompt.md -->
You review diffs in this repository against the conventions below. Flag each
deviation with the file and line, cite the rule's document and section, and
name the fix. Do not restate rules a change already follows.

## Module boundaries (AGENTS.md)

- `src/api` never imports from `src/ui`. Flag any new import across that line.
- Logic both apps use belongs in `src/shared`, not in a copy per app. Flag a
  second copy and name the module it belongs in. Do not flag a pair listed in
  `scripts/allowed-duplicates.txt` with its reason.

## Testing (docs/testing.md)

- Component tests render through `test/render.tsx`. Flag direct imports of
  the testing library's `render` in component tests.
```

Keep a deterministic check (a lint rule, a script, a test) for anything a
machine can verify, and use the prompt for the judgment calls around it; tell
the reviewer to treat the check's output as a floor, not a ceiling. A docs
index that marks each document as current or in progress helps reviewers know
which documents to trust.

## Writing prompts that hold up

Every prompt in this catalog shares a review bar: report only what the PR
introduces or worsens, with a concrete consequence and the input or sequence
that shows it, stated as confirmed or inferred, actionable from the changed
code, and not already caught by a tool the repository runs. Zero findings is a
normal result. Keeping that bar in each prompt, plus a "What not to report"
list and a severity scale written for the agent's lane, is what keeps reviews
short and believable. Agents stay in their own lane without naming other
agents; when two lanes report the same problem, the rollup merges them.
