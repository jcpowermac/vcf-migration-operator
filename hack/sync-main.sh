#!/usr/bin/env bash
#
# Deterministically sync upstream/main into an upstream release branch by
# cherry-picking every commit merged to main since the target branch
# diverged, excluding PRs labeled "konflux-nudge" or titled "chore(deps)*"
# plus any PRs named with --exclude-pr. Pushes the sync branch to the fork
# and opens a PR against the target branch.
#
# Usage:
#   hack/sync-main.sh v0.1 --list   print the planned commits and exit
#   hack/sync-main.sh v0.1          print plan, confirm, pick, push, open PR
#   hack/sync-main.sh v0.1 --yes    same, without the confirmation prompt
#
#   --exclude-pr <num> can be given repeatedly (or as a space-separated list)
# to drop specific PRs from the plan, e.g. --exclude-pr 105 --exclude-pr "17 42"
#
# Determinism: candidates are taken from a first-parent walk of
# TARGET_REF..upstream/main (oldest first), so the same upstream state
# always produces the same ordered plan. A PR lookup that cannot be
# resolved is fatal rather than silently included.
#
# Resume: the picks happen in a worktree at .git/sync-main-to-<target>, so
# the invoking checkout is never switched and this script stays on disk (the
# target branch does not contain it). On a conflict the script stops with the
# cherry-pick in progress in that worktree: resolve it there, run
# `git cherry-pick --continue`, then rerun; commits already on the sync
# branch are skipped by the "(cherry picked from commit ...)" trailer that
# -x writes, which survives conflict resolutions.
#
# Limitations: only the commits each merge introduced are picked; a merge
# commit's own resolution delta is not applied. Label filtering relies on
# PRs landing as "Merge pull request #N" merge commits (verified for this
# repo: main's first-parent chain since v0.1 contains only merge commits,
# and every konflux-nudge PR is titled "chore(deps): ...").

set -euo pipefail

UPSTREAM_REMOTE=upstream
FORK_REMOTE=origin
UPSTREAM_REPO=openshift/vcf-migration-operator
SOURCE_REF="$UPSTREAM_REMOTE/main"

script_name=${0##*/}
target_branch=
list_only=no
confirm=yes
exclude_prs=" "

usage() {
    printf 'Usage: %s <target-branch> [--list|--yes] [--exclude-pr <num>...]\n' "$script_name" >&2
    printf '  --list         print the planned commits and exit\n' >&2
    printf '  --yes          skip the confirmation prompt\n' >&2
    printf '  --exclude-pr   PR number to skip; repeat or space-separate several\n' >&2
}

fail() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

while [ $# -gt 0 ]; do
    case $1 in
        --list) list_only=yes ;;
        --yes) confirm=no ;;
        --exclude-pr)
            [ $# -ge 2 ] || fail "--exclude-pr requires a PR number"
            exclude_prs="$exclude_prs$2 "
            shift
            ;;
        -h|--help) usage; exit 0 ;;
        -*) usage; fail "unknown argument: $1" ;;
        *)
            [ -z "$target_branch" ] || fail "only one target branch allowed"
            target_branch=$1
            ;;
    esac
    shift
done
[ -n "$target_branch" ] || { usage; fail "target branch required, e.g. v0.1"; }

command -v gh >/dev/null 2>&1 || fail "gh CLI is required"
case $(git remote get-url "$UPSTREAM_REMOTE") in
    *"$UPSTREAM_REPO"*) ;;
    *) fail "remote $UPSTREAM_REMOTE does not point at $UPSTREAM_REPO" ;;
esac

TARGET_REF="$UPSTREAM_REMOTE/$target_branch"
BRANCH="sync-main-to-$target_branch"
# pwd -P: git worktree list reports symlink-free paths (/tmp -> /private/tmp)
worktree_dir="$(cd "$(git rev-parse --git-common-dir)" && pwd -P)/$BRANCH"

# Validate reused worktrees before fast-forwarding or publishing, even with no picks left.
if [ -e "$worktree_dir" ]; then
    git worktree list --porcelain | grep -qxF "worktree $worktree_dir" ||
        fail "$worktree_dir exists but is not a registered worktree; inspect it before rerunning"
    [ "$(git -C "$worktree_dir" symbolic-ref -q HEAD)" = "refs/heads/$BRANCH" ] ||
        fail "$worktree_dir must have $BRANCH checked out; restore that branch and rerun"
    if git -C "$worktree_dir" rev-parse -q --verify CHERRY_PICK_HEAD >/dev/null; then
        fail "a cherry-pick is still in progress; finish it with 'git -C \"$worktree_dir\" cherry-pick --continue' (or --skip / --abort), then rerun"
    fi
    [ -z "$(git -C "$worktree_dir" status --porcelain)" ] ||
        fail "$worktree_dir has uncommitted changes; commit, stash or discard them and rerun"
fi

git fetch --prune "$UPSTREAM_REMOTE" --quiet
git rev-parse -q --verify "$TARGET_REF" >/dev/null ||
    fail "branch $target_branch not found on $UPSTREAM_REMOTE"

# Excluded when labeled konflux-nudge or titled chore(deps)*. An
# unresolvable lookup is fatal: including on a transient error would make
# the plan differ between runs of the same upstream state.
pr_excluded() {
    local info title labels
    info=$(gh pr view "$1" --repo "$UPSTREAM_REPO" --json title,labels \
        --jq '[.title, (.labels | map(.name) | join(","))] | @tsv') ||
        fail "cannot look up PR #$1"
    IFS=$'\t' read -r title labels <<<"$info"
    case ",$labels," in
        *,konflux-nudge,*) return 0 ;;
    esac
    case $title in
        chore\(deps\)*) return 0 ;;
    esac
    return 1
}

has_branch=no
fork_ref="refs/remotes/$FORK_REMOTE/$BRANCH"
has_fork_branch=no
if git fetch -q "$FORK_REMOTE" "+$BRANCH:$fork_ref" 2>/dev/null; then
    has_fork_branch=yes
fi
if git show-ref --verify --quiet "refs/heads/$BRANCH"; then
    if [ "$has_fork_branch" = yes ]; then
        if git merge-base --is-ancestor "$BRANCH" "$fork_ref"; then
            if [ "$(git symbolic-ref -q HEAD)" = "refs/heads/$BRANCH" ]; then
                git merge --ff-only "$fork_ref"
            elif [ -d "$worktree_dir" ]; then
                git -C "$worktree_dir" merge --ff-only "$fork_ref"
            else
                git branch -f "$BRANCH" "$fork_ref"
            fi
        elif ! git merge-base --is-ancestor "$fork_ref" "$BRANCH"; then
            fail "$BRANCH and $FORK_REMOTE/$BRANCH have diverged; reconcile manually"
        fi
    fi
    has_branch=yes
elif [ "$has_fork_branch" = yes ]; then
    # local branch deleted but the fork still has the picks; rebuild local
    git branch "$BRANCH" "$fork_ref"
    has_branch=yes
fi

if [ "$has_branch" = yes ]; then
    merge_base=$(git merge-base "$TARGET_REF" "$BRANCH")
    applied_refs=("$TARGET_REF" "$BRANCH")
else
    merge_base=$(git rev-parse "$TARGET_REF")
    applied_refs=("$TARGET_REF")
fi
# Include merged syncs on the target as well as outstanding sync picks.
applied_shas=$(git log --format=%B "${applied_refs[@]}" |
    sed -n 's/^(cherry picked from commit \([0-9a-f]\{7,\}\))$/\1/p')

already_applied() {
    grep -qxF "$(git rev-parse "$1")" <<<"$applied_shas"
}

plan=()
skipped=()

add_commit() {
    local sha=$1
    if already_applied "$sha"; then
        printf 'already applied: %s %s\n' \
            "$(git rev-parse --short "$sha")" "$(git log -1 --format=%s "$sha")" >&2
        return
    fi
    plan+=("$sha")
}

# First-parent walk, oldest first. Merge commits resolve to their PR; the
# commits each merge introduced are picked individually.
candidates=$(git rev-list --reverse --first-parent "$TARGET_REF..$SOURCE_REF") ||
    fail "cannot enumerate $TARGET_REF..$SOURCE_REF"
for sha in $candidates; do
    subject=$(git log -1 --format=%s "$sha")
    if git rev-parse -q --verify "$sha^2" >/dev/null; then
        num=${subject#Merge pull request #}
        if [ "$num" != "$subject" ]; then
            num=${num%% *}
            case $num in ''|*[!0-9]*) num= ;; esac
        else
            num=
        fi
        if [ -n "$num" ]; then
            case "$exclude_prs" in
                *" $num "*)
                    skipped+=("$(git rev-parse --short "$sha") #$num (excluded)")
                    continue
                    ;;
            esac
            if pr_excluded "$num"; then
                skipped+=("$(git rev-parse --short "$sha") #$num $subject")
                continue
            fi
        fi
        inner=()
        while read -r c; do
            inner+=("$c")
        done < <(git rev-list --reverse --no-merges "$sha" "^$sha^1" "^$TARGET_REF")
        # empty-array guard: "${inner[@]}" aborts under set -u on bash < 4.4
        if [ "${#inner[@]}" -gt 0 ]; then
            for c in "${inner[@]}"; do
                add_commit "$c"
            done
        else
            printf 'warning: merge %s introduced no new commits for %s\n' \
                "$sha" "$target_branch" >&2
        fi
    else
        # Direct commit on main (not via a merge commit); konflux direct
        # pushes are titled chore(deps). Labels cannot be checked without
        # a PR number, and this repo merges all PRs as merge commits.
        case $subject in
            chore\(deps\)*)
                skipped+=("$(git rev-parse --short "$sha") (direct) $subject")
                continue
                ;;
        esac
        add_commit "$sha"
    fi
done

if [ "${#plan[@]}" -eq 0 ]; then
    printf 'nothing new to cherry-pick\n'
else
    printf 'Will cherry-pick %d commits (oldest first):\n' "${#plan[@]}"
    for sha in "${plan[@]}"; do
        printf '  %s %s\n' "$(git rev-parse --short "$sha")" "$(git log -1 --format=%s "$sha")"
    done
fi
if [ "${#skipped[@]}" -gt 0 ]; then
    printf '\nSkipped (konflux-nudge / chore(deps)):\n'
    printf '  %s\n' "${skipped[@]}"
fi

if [ "$list_only" = yes ]; then
    exit 0
fi

# an empty plan can still mean unfinished work: a previous run may have
# picked everything and failed at the push or PR step
if [ "${#plan[@]}" -eq 0 ]; then
    if [ "$has_branch" != yes ] ||
            [ "$(git rev-list --count "$TARGET_REF..$BRANCH")" -eq 0 ]; then
        exit 0
    fi
    printf 'nothing new to cherry-pick; finishing push and PR\n'
fi

if [ "$confirm" = yes ]; then
    read -r -p "Proceed? [y/N] " answer
    case $answer in
        y|Y*) ;;
        *) printf 'aborted\n'; exit 0 ;;
    esac
fi

# an empty plan still means there is nothing to pick, so no worktree needed
if [ "${#plan[@]}" -gt 0 ]; then
    # shortcut: assumes the target branch is never force-pushed; if it
    # advanced, trailer skipping keeps picks correct but the branch may
    # need a manual rebase
    if [ "$has_branch" = yes ] &&
            [ "$merge_base" != "$(git rev-parse "$TARGET_REF")" ]; then
        printf 'warning: %s has moved since %s was created\n' \
            "$TARGET_REF" "$BRANCH" >&2
    fi

    git worktree prune
    if [ ! -d "$worktree_dir" ]; then
        elsewhere=$(git worktree list --porcelain |
            awk -v ref="refs/heads/$BRANCH" '/^worktree /{p=$2} $0 == "branch " ref {print p}')
        [ -z "$elsewhere" ] ||
            fail "$BRANCH is checked out at $elsewhere; switch that worktree to another branch and rerun"
        if [ "$has_branch" = yes ]; then
            git worktree add --quiet "$worktree_dir" "$BRANCH"
        else
            git worktree add --quiet -b "$BRANCH" "$worktree_dir" "$TARGET_REF"
        fi
    fi

    cd "$worktree_dir"

    for ((i = 0; i < ${#plan[@]}; i++)); do
        sha=${plan[i]}
        printf 'picking %s %s\n' \
            "$(git rev-parse --short "$sha")" "$(git log -1 --format=%s "$sha")"
        if ! git cherry-pick -x "$sha"; then
            # an empty pick means the change is already present; skip it
            if git diff --cached --quiet && [ -z "$(git ls-files -u)" ]; then
                git cherry-pick --skip
                continue
            fi
            cat >&2 <<EOF

Conflict while cherry-picking $sha in the sync worktree:
    $worktree_dir

Resolve the conflict in that worktree, then:
    git -C "$worktree_dir" cherry-pick --continue
(use --skip if the resolution leaves the commit empty) and rerun this
script; already-applied commits are skipped.

To discard this run:
    git -C "$worktree_dir" cherry-pick --abort
EOF
            exit 1
        fi
    done
fi

if [ "$(git rev-list --count "$TARGET_REF..$BRANCH")" -eq 0 ]; then
    printf 'nothing to push; sync branch has no commits ahead of %s\n' "$TARGET_REF"
    exit 0
fi

fork_owner=$(git remote get-url "$FORK_REMOTE" |
    sed -E 's#.*github\.com[/:]##; s#/.*##')
[ -n "$fork_owner" ] || fail "cannot derive fork owner from $FORK_REMOTE remote URL"
git push -u "$FORK_REMOTE" "$BRANCH"

existing=$(gh api --method GET "repos/$UPSTREAM_REPO/pulls" \
    -f state=open -f head="$fork_owner:$BRANCH" -f per_page=1) ||
    fail "cannot query open PRs for $BRANCH"
case $existing in
    *'"number"'*)
        printf 'PR already open for %s, done\n' "$BRANCH"
        exit 0
        ;;
esac

count=$(git rev-list --count --no-merges "$merge_base..$BRANCH")
body=$(printf 'Cherry-picked %s commits from main into %s, excluding PRs labeled konflux-nudge or titled chore(deps).\n\nCommits:\n%s\n' \
    "$count" "$target_branch" "$(git log --reverse --format='- %h %s' "$merge_base..$BRANCH")")

gh pr create \
    --repo "$UPSTREAM_REPO" \
    --base "$target_branch" \
    --head "$fork_owner:$BRANCH" \
    --title "Sync main into $target_branch ($count commits)" \
    --body "$body"
