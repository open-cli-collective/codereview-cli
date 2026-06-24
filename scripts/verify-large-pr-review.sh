#!/usr/bin/env bash
set -euo pipefail

default_target="https://github.com/open-cli-collective/codereview-cli/pull/358"
default_max_context_bytes="524288"
default_review_timeout_seconds="1200"

usage() {
  cat <<'EOF'
Verify large-PR review does not regress into context stuffing.

Usage:
  scripts/verify-large-pr-review.sh [--self-test] [--help]

Real mode builds the local cr binary, runs a no-post JSON review twice against a
large PR, and validates run artifacts and stderr breadcrumbs. It leaves normal
cr config resolution alone, but isolates data/cache state in a temporary
workspace shared by the two passes. Temporary state is deleted unless
CR_LARGE_PR_KEEP_TMP is set. The GitHub no-post side-effect check expects a
quiescent PR while the harness is running.

Environment:
  CR_LARGE_PR_REVIEW_TARGET     PR URL to review.
                                Default: https://github.com/open-cli-collective/codereview-cli/pull/358
  CR_LARGE_PR_MAX_PROMPT_BYTES  Max bytes for each persisted model-facing context artifact.
                                Default: 524288
  CR_LARGE_PR_SENTINEL          Source sentinel expected in diff/workbench artifacts
                                and forbidden in model-facing artifacts. If empty,
                                the script derives a long added diff line.
  CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS
                                Max seconds for each no-post review pass.
                                Default: 1200
  CR_LARGE_PR_KEEP_TMP          Keep temporary state and print its path.

Examples:
  scripts/verify-large-pr-review.sh --self-test
  scripts/verify-large-pr-review.sh
  CR_LARGE_PR_REVIEW_TARGET=https://github.com/owner/repo/pull/123 scripts/verify-large-pr-review.sh
EOF
}

fail() {
  local invariant="$1"
  shift
  printf 'FAIL invariant=%s: %s\n' "$invariant" "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*" >&2
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing_tool" "required command not found: $1"
}

file_size() {
  wc -c <"$1" | tr -d '[:space:]'
}

assert_file_exists() {
  local invariant="$1"
  local path="$2"
  [[ -e "$path" ]] || fail "$invariant" "missing $path"
}

assert_file_contains() {
  local invariant="$1"
  local path="$2"
  local needle="$3"
  grep -F -- "$needle" "$path" >/dev/null || fail "$invariant" "$path does not contain expected sentinel"
}

assert_file_omits() {
  local invariant="$1"
  local path="$2"
  local needle="$3"
  if grep -F -- "$needle" "$path" >/dev/null; then
    fail "$invariant" "$path contains forbidden content: $needle"
  fi
}

json_field() {
  local json_path="$1"
  local field_path="$2"
  go run ./scripts/review-json-field.go "$json_path" "$field_path"
}

derive_sentinel() {
  local diff_path="$1"
  awk '
    /^(\+\+\+|---)/ { next }
    /^\+/ {
      line = substr($0, 2)
      if (length(line) >= 120 && length(line) > length(best)) {
        best = line
      }
    }
    END {
      if (best != "") {
        print best
      }
    }
  ' "$diff_path"
}

model_facing_files() {
  local artifact_dir="$1"
  local candidate
  for candidate in "$artifact_dir/llm-tasks" "$artifact_dir/agent-logs" "$artifact_dir/dossier/final" "$artifact_dir/dossier/summary"; do
    if [[ -d "$candidate" ]]; then
      find "$candidate" -type f
    fi
  done
  for candidate in "$artifact_dir/findings.json" "$artifact_dir/rollup.md"; do
    if [[ -f "$candidate" ]]; then
      printf '%s\n' "$candidate"
    fi
  done
}

assert_artifact_shape() {
  local artifact_dir="$1"
  assert_file_exists "artifact_root_exists" "$artifact_dir"
  assert_file_exists "raw_diff_exists" "$artifact_dir/diff.patch"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/index.json"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/summary"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/final/pr-intent.md"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/final/change-map.md"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/final/repo-guidance.md"
  assert_file_exists "dossier_exists" "$artifact_dir/dossier/final/discussion.md"
  assert_file_exists "workbench_exists" "$artifact_dir/workbench/metadata.json"
  assert_file_exists "workbench_exists" "$artifact_dir/workbench/repo"
  assert_file_exists "workbench_exists" "$artifact_dir/workbench/scratch"
}

assert_context_artifacts() {
  local artifact_dir="$1"
  local sentinel="$2"
  local max_bytes="$3"
  local file
  local size
  local found_any=0

  while IFS= read -r file; do
    [[ -n "$file" ]] || continue
    found_any=1
    size="$(file_size "$file")"
    if (( size > max_bytes )); then
      fail "context_size_limit" "$file is ${size} bytes; limit is ${max_bytes}"
    fi
    assert_file_omits "full_diff_absent" "$file" "diff --git "
    assert_file_omits "full_diff_absent" "$file" "@@ -"
    assert_file_omits "legacy_file_body_absent" "$file" '"base_content"'
    assert_file_omits "legacy_file_body_absent" "$file" '"head_content"'
    if [[ -n "$sentinel" ]]; then
      assert_file_omits "sentinel_absent" "$file" "$sentinel"
    fi
  done < <(model_facing_files "$artifact_dir")

  (( found_any == 1 )) || fail "model_facing_artifacts_exist" "no model-facing artifacts found under $artifact_dir"
}

assert_sentinel_source() {
  local artifact_dir="$1"
  local sentinel="$2"
  [[ -n "$sentinel" ]] || fail "sentinel_source_exists" "no source sentinel supplied or derived"
  assert_file_contains "sentinel_source_exists" "$artifact_dir/diff.patch" "$sentinel"
  grep -R -F -- "$sentinel" "$artifact_dir/workbench/repo" >/dev/null || fail "sentinel_source_exists" "workbench repo does not contain expected sentinel"
}

github_pr_parts() {
  local target="$1"
  sed -nE 's#^https://github[.]com/([^/]+)/([^/]+)/pull/([0-9]+).*#\1 \2 \3#p' <<<"$target"
}

github_post_snapshot() {
  local target="$1"
  local output_path="$2"
  local parts
  local owner
  local repo
  local number

  parts="$(github_pr_parts "$target")"
  [[ -n "$parts" ]] || fail "post_snapshot_supported" "post snapshot requires a github.com pull request URL"
  read -r owner repo number <<<"$parts"
  {
    gh api --paginate "repos/$owner/$repo/issues/$number/comments" --jq '.[] | {kind:"issue-comment", id, updated_at, body} | @json'
    gh api --paginate "repos/$owner/$repo/pulls/$number/reviews" --jq '.[] | {kind:"review", id, state, commit_id, submitted_at, body} | @json'
    gh api --paginate "repos/$owner/$repo/pulls/$number/comments" --jq '.[] | {kind:"review-comment", id, updated_at, body, path, line, side, commit_id} | @json'
  } | sort >"$output_path"
}

assert_post_snapshot_unchanged() {
  local before_path="$1"
  local after_path="$2"
  if ! cmp -s "$before_path" "$after_path"; then
    fail "no_post_side_effects" "GitHub comments/reviews changed during no-post harness run; before=$before_path after=$after_path"
  fi
}

task_ids_for_op() {
  local stderr_path="$1"
  local op="$2"
  awk -v op="op=\""${op}"\"" '
    index($0, op) && match($0, /task_id="[^"]+"/) {
      value = substr($0, RSTART + 9, RLENGTH - 10)
      print value
    }
  ' "$stderr_path" | sort -u
}

id_set_contains() {
  local haystack="$1"
  local needle="$2"
  grep -Fx -- "$needle" <<<"$haystack" >/dev/null
}

assert_breadcrumbs() {
  local first_stderr="$1"
  local second_stderr="$2"
  local first_ids
  local second_load_ids
  local second_run_ids
  local id

  grep -F 'op="run_llm_task"' "$first_stderr" >/dev/null || fail "run_llm_task_breadcrumb" "first run stderr has no run_llm_task breadcrumb"
  grep -F 'op="load_llm_task"' "$second_stderr" >/dev/null || fail "load_llm_task_breadcrumb" "second run stderr has no load_llm_task breadcrumb"

  first_ids="$(task_ids_for_op "$first_stderr" "run_llm_task")"
  second_load_ids="$(task_ids_for_op "$second_stderr" "load_llm_task")"
  second_run_ids="$(task_ids_for_op "$second_stderr" "run_llm_task")"
  [[ -n "$first_ids" ]] || fail "breadcrumb_task_ids" "first run has no task_id on run_llm_task breadcrumbs"
  [[ -n "$second_load_ids" ]] || fail "breadcrumb_task_ids" "second run has no task_id on load_llm_task breadcrumbs"

  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if id_set_contains "$second_run_ids" "$id"; then
      fail "durable_task_reused" "second run reran unchanged task_id $id"
    fi
    if ! id_set_contains "$second_load_ids" "$id"; then
      fail "durable_task_reused" "second run did not load unchanged task_id $id"
    fi
  done <<<"$first_ids"
}

terminate_process_tree() {
  local pid="$1"
  local child
  while IFS= read -r child; do
    [[ -n "$child" ]] || continue
    terminate_process_tree "$child"
  done < <(pgrep -P "$pid" 2>/dev/null || true)
  kill "$pid" >/dev/null 2>&1 || true
}

run_review() {
  local label="$1"
  local target="$2"
  local cr_bin="$3"
  local stdout_path="$4"
  local stderr_path="$5"
  local timeout_seconds="${CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS:-$default_review_timeout_seconds}"
  local interval_seconds=5
  local elapsed_seconds=0
  local pid

  note "Running ${label} dry-run review against ${target}"
  "$cr_bin" review "$target" --no-post --json >"$stdout_path" 2>"$stderr_path" &
  pid=$!
  while kill -0 "$pid" >/dev/null 2>&1; do
    if (( elapsed_seconds >= timeout_seconds )); then
      terminate_process_tree "$pid"
      sleep 2
      if kill -0 "$pid" >/dev/null 2>&1; then
        while IFS= read -r child; do
          [[ -n "$child" ]] || continue
          kill -KILL "$child" >/dev/null 2>&1 || true
        done < <(pgrep -P "$pid" 2>/dev/null || true)
        kill -KILL "$pid" >/dev/null 2>&1 || true
      fi
      wait "$pid" >/dev/null 2>&1 || true
      fail "review_${label}_timeout" "cr review exceeded ${timeout_seconds}s; stdout=$stdout_path stderr=$stderr_path"
    fi
    sleep "$interval_seconds"
    elapsed_seconds=$((elapsed_seconds + interval_seconds))
  done
  if ! wait "$pid"; then
    fail "review_${label}" "cr review failed; stdout=$stdout_path stderr=$stderr_path"
  fi
}

run_self_test() {
  local tmp
  local artifact_dir
  local sentinel
  local negative_output
  local negative_status
  local first_stderr
  local second_stderr
  local partial_second_stderr
  local before_posts
  local after_posts
  local fake_cr
  local fake_stdout
  local fake_stderr
  local timeout_marker

  tmp="$(mktemp -d "${TMPDIR:-/tmp}/cr-large-pr-self-test.XXXXXX")"
  trap "rm -rf '$tmp'" EXIT
  artifact_dir="$tmp/artifacts"
  sentinel="CR_LARGE_PR_SELF_TEST_SENTINEL_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_repeated_body_line_for_context_stuffing_detection"

  mkdir -p "$artifact_dir/dossier/final" "$artifact_dir/dossier/summary" "$artifact_dir/workbench/repo" "$artifact_dir/workbench/scratch" "$artifact_dir/llm-tasks/orchestrator-selection" "$artifact_dir/agent-logs"
  printf 'diff --git a/large.go b/large.go\n@@ -1 +1 @@\n+%s\n' "$sentinel" >"$artifact_dir/diff.patch"
  printf '%s\n' "$sentinel" >"$artifact_dir/workbench/repo/large.go"
  printf '{"artifacts":["final/pr-intent.md"]}\n' >"$artifact_dir/dossier/index.json"
  printf 'Intent only.\n' >"$artifact_dir/dossier/final/pr-intent.md"
  printf 'large.go modified\n' >"$artifact_dir/dossier/final/change-map.md"
  printf 'Use repo guidance.\n' >"$artifact_dir/dossier/final/repo-guidance.md"
  printf 'No relevant discussion.\n' >"$artifact_dir/dossier/final/discussion.md"
  printf '{"repo_path":"%s","scratch_path":"%s"}\n' "$artifact_dir/workbench/repo" "$artifact_dir/workbench/scratch" >"$artifact_dir/workbench/metadata.json"
  printf '{"task_id":"orchestrator-selection","phase":"selection","status":"succeeded"}\n' >"$artifact_dir/llm-tasks/orchestrator-selection/metadata.json"
  printf '{"selected_agents":[]}\n' >"$artifact_dir/llm-tasks/orchestrator-selection/validated-output.json"
  printf 'model response without stuffed source\n' >"$artifact_dir/agent-logs/orchestrator-selection.jsonl"
  printf '[]\n' >"$artifact_dir/findings.json"
  printf 'No findings.\n' >"$artifact_dir/rollup.md"

  assert_artifact_shape "$artifact_dir"
  assert_sentinel_source "$artifact_dir" "$sentinel"
  assert_context_artifacts "$artifact_dir" "$sentinel" "$default_max_context_bytes"

  printf '%s\n' "$sentinel" >>"$artifact_dir/agent-logs/orchestrator-selection.jsonl"
  set +e
  negative_output="$(assert_context_artifacts "$artifact_dir" "$sentinel" "$default_max_context_bytes" 2>&1)"
  negative_status=$?
  set -e
  if (( negative_status == 0 )); then
    fail "self_test_negative" "stuffed sentinel fixture unexpectedly passed"
  fi
  grep -F 'invariant=sentinel_absent' <<<"$negative_output" >/dev/null || fail "self_test_negative" "negative fixture did not name sentinel_absent; output: $negative_output"

  first_stderr="$tmp/first.stderr.log"
  second_stderr="$tmp/second.stderr.log"
  partial_second_stderr="$tmp/partial-second.stderr.log"
  {
    printf 'cr progress event=start command="review" op="run_llm_task" target="llm_task" task_id="orchestrator-selection"\n'
    printf 'cr progress event=start command="review" op="run_llm_task" target="llm_task" task_id="orchestrator-rollup"\n'
  } >"$first_stderr"
  {
    printf 'cr progress event=start command="review" op="load_llm_task" target="llm_task" task_id="orchestrator-selection"\n'
    printf 'cr progress event=start command="review" op="load_llm_task" target="llm_task" task_id="orchestrator-rollup"\n'
  } >"$second_stderr"
  assert_breadcrumbs "$first_stderr" "$second_stderr"

  {
    printf 'cr progress event=start command="review" op="load_llm_task" target="llm_task" task_id="orchestrator-selection"\n'
    printf 'cr progress event=start command="review" op="run_llm_task" target="llm_task" task_id="orchestrator-rollup"\n'
  } >"$partial_second_stderr"
  set +e
  negative_output="$(assert_breadcrumbs "$first_stderr" "$partial_second_stderr" 2>&1)"
  negative_status=$?
  set -e
  if (( negative_status == 0 )); then
    fail "self_test_negative" "partial durable reuse fixture unexpectedly passed"
  fi
  grep -F 'invariant=durable_task_reused' <<<"$negative_output" >/dev/null || fail "self_test_negative" "partial reuse fixture did not name durable_task_reused; output: $negative_output"

  before_posts="$tmp/before-posts.txt"
  after_posts="$tmp/after-posts.txt"
  printf 'issue-comment:1\nreview:2\nreview-comment:3\n' >"$before_posts"
  printf 'issue-comment:1\nreview:2\nreview-comment:3\n' >"$after_posts"
  assert_post_snapshot_unchanged "$before_posts" "$after_posts"
  printf 'issue-comment:1\nreview:2\nreview-comment:3\nreview:4\n' >"$after_posts"
  set +e
  negative_output="$(assert_post_snapshot_unchanged "$before_posts" "$after_posts" 2>&1)"
  negative_status=$?
  set -e
  if (( negative_status == 0 )); then
    fail "self_test_negative" "post side-effect fixture unexpectedly passed"
  fi
  grep -F 'invariant=no_post_side_effects' <<<"$negative_output" >/dev/null || fail "self_test_negative" "post side-effect fixture did not name no_post_side_effects; output: $negative_output"

  fake_cr="$tmp/fake-cr"
  fake_stdout="$tmp/fake.stdout.json"
  fake_stderr="$tmp/fake.stderr.log"
  timeout_marker="cr-large-pr-timeout-child-$$"
  cat >"$fake_cr" <<EOF
#!/usr/bin/env bash
bash -c 'sleep 60' "$timeout_marker" &
sleep 60
EOF
  chmod +x "$fake_cr"
  set +e
  negative_output="$(CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS=1 run_review "timeout" "https://github.com/open-cli-collective/codereview-cli/pull/358" "$fake_cr" "$fake_stdout" "$fake_stderr" 2>&1)"
  negative_status=$?
  set -e
  if (( negative_status == 0 )); then
    fail "self_test_negative" "timeout fixture unexpectedly passed"
  fi
  grep -F 'invariant=review_timeout_timeout' <<<"$negative_output" >/dev/null || fail "self_test_negative" "timeout fixture did not name review_timeout_timeout; output: $negative_output"
  if pgrep -f "$timeout_marker" >/dev/null 2>&1; then
    pkill -f "$timeout_marker" >/dev/null 2>&1 || true
    fail "self_test_negative" "timeout fixture left a child process running"
  fi

  note "Self-test passed"
}

run_real() {
  require_command go
  require_command git
  require_command awk
  require_command find
  require_command gh
  require_command grep
  require_command pgrep

  local target="${CR_LARGE_PR_REVIEW_TARGET:-$default_target}"
  local max_bytes="${CR_LARGE_PR_MAX_PROMPT_BYTES:-$default_max_context_bytes}"
  local timeout_seconds="${CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS:-$default_review_timeout_seconds}"
  local tmp
  local gobin
  local cr_bin
  local first_stdout
  local first_stderr
  local second_stdout
  local second_stderr
  local first_artifacts
  local second_artifacts
  local sentinel="${CR_LARGE_PR_SENTINEL:-}"
  local before_posts
  local after_posts

  [[ "$max_bytes" =~ ^[0-9]+$ ]] || fail "invalid_limit" "CR_LARGE_PR_MAX_PROMPT_BYTES must be an integer"
  [[ "$timeout_seconds" =~ ^[0-9]+$ ]] || fail "invalid_timeout" "CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS must be an integer"
  (( timeout_seconds > 0 )) || fail "invalid_timeout" "CR_LARGE_PR_REVIEW_TIMEOUT_SECONDS must be positive"

  tmp="$(mktemp -d "${TMPDIR:-/tmp}/cr-large-pr-review.XXXXXX")"
  if [[ -z "${CR_LARGE_PR_KEEP_TMP:-}" ]]; then
    trap "rm -rf '$tmp'" EXIT
  else
    note "Keeping temporary workspace: $tmp"
  fi

  gobin="$tmp/gobin"
  mkdir -p "$gobin" "$tmp/run" "$tmp/data-home" "$tmp/cache-home"
  note "Building local cr binary"
  GOBIN="$gobin" go install ./cmd/cr
  cr_bin="$gobin/cr"

  first_stdout="$tmp/run/first.stdout.json"
  first_stderr="$tmp/run/first.stderr.log"
  second_stdout="$tmp/run/second.stdout.json"
  second_stderr="$tmp/run/second.stderr.log"
  before_posts="$tmp/run/before-posts.txt"
  after_posts="$tmp/run/after-posts.txt"

  github_post_snapshot "$target" "$before_posts"
  XDG_DATA_HOME="$tmp/data-home" XDG_CACHE_HOME="$tmp/cache-home" run_review "first" "$target" "$cr_bin" "$first_stdout" "$first_stderr"
  first_artifacts="$(json_field "$first_stdout" "run.artifact_path")"
  assert_artifact_shape "$first_artifacts"

  if [[ -z "$sentinel" ]]; then
    sentinel="$(derive_sentinel "$first_artifacts/diff.patch")"
  fi
  assert_sentinel_source "$first_artifacts" "$sentinel"
  assert_context_artifacts "$first_artifacts" "$sentinel" "$max_bytes"

  XDG_DATA_HOME="$tmp/data-home" XDG_CACHE_HOME="$tmp/cache-home" run_review "second" "$target" "$cr_bin" "$second_stdout" "$second_stderr"
  second_artifacts="$(json_field "$second_stdout" "run.artifact_path")"
  assert_artifact_shape "$second_artifacts"
  assert_sentinel_source "$second_artifacts" "$sentinel"
  assert_context_artifacts "$second_artifacts" "$sentinel" "$max_bytes"
  assert_breadcrumbs "$first_stderr" "$second_stderr"
  github_post_snapshot "$target" "$after_posts"
  assert_post_snapshot_unchanged "$before_posts" "$after_posts"

  note "Large-PR review harness passed"
  note "First artifacts: $first_artifacts"
  note "Second artifacts: $second_artifacts"
}

main() {
  case "${1:-}" in
    --help|-h)
      usage
      ;;
    --self-test)
      run_self_test
      ;;
    "")
      run_real
      ;;
    *)
      usage >&2
      fail "usage" "unknown argument: $1"
      ;;
  esac
}

main "$@"
