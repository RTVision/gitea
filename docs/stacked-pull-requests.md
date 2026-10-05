# Stacked pull requests

A stack is an ordered chain of pull requests owned by the repository containing
its trunk. Source branches can live in that repository or in one direct fork with
the same visibility. In the fork workflow, the first PR targets the main repository;
later PRs belong to the fork and target the preceding source branch. Each layer
lands into the main repository's trunk. The trunk can be any repository branch.

Each pull request retains its own discussion, reviews and diff. Required reviews,
status checks, protected-file rules and CODEOWNERS are evaluated against the
trunk's policy. Permission to rewrite a layer's source branch is checked against
that source branch's protection rules.

## Enable stacks

Set the following instance configuration and restart Gitea:

```ini
[repository.pull-request]
ENABLE_STACKS = true
```

Creation and append are disabled by default. Disabling the setting later prevents
new stacks and additions; existing stacks remain readable and their operations
and recovery controls remain available. Stacks use additive database tables.
Back up the database and repositories together before upgrading an instance.

## Create and review

Create ordinary pull requests whose branches form a chain, then open **Stacks**
from the repository's pull request list. Select **New stack** and search for the
last pull request in the chain. The form follows base branches down, stopping at
the default branch, a head branch shared by several open pull requests, or a
layer of another open stack, and lists the chain in landing order. It starts the
stack just above the highest branch that several pull requests build on; pick
another start layer to include more or fewer layers. The stack lands into the
start layer's base branch. Layers that are behind their parent or contain merge
commits are flagged, since only merge mode accepts them. Choose the stack mode.

For example, with `release` as the trunk:

```text
release ← feature/storage ← feature/api ← feature/ui
          PR 41             PR 42         PR 43
```

Each branch needs at least one commit beyond its parent. In rebase mode, each
branch must also contain its current parent's head and have linear history. In
merge mode, a branch may be behind its parent; **Update stack** merges the parent
in. Source branches from several repositories, duplicate membership, multiple open
pull requests sharing a head branch, and already scheduled ordinary auto-merges
cannot be adopted.

Fork PRs retain their repository, number, discussion and immediate base after
lower layers land. The stack records the main repository's trunk as their effective
landing target. Merged PR payloads expose `merged_repo_id` and `merged_branch` so
clients can locate the published commit. All source rewrites still require
permission on the fork branch; write access to the main repository does not grant
write access to the fork.

Contributors without write access to the main repository can create, extend,
synchronize, rebase, update and unstack their own fork layers. They must be the
author of every layer and have push permission on every source branch. Creating
the stack on their behalf does not change these checks. Landing still requires
merge permission on the main repository's trunk.

The collapsible stack box on each pull request shows connected layers from top to
bottom, with the trunk branch below them. Review the
individual layer diff as usual. A closed but unmerged predecessor does not satisfy
the stack's landing order.

The layer dropdown, stack box and stack page show each layer's latest checks and
official review counts, matching the pull request list. These indicators summarize
existing checks and reviews; the stack page lists any remaining landing blockers.

The repository's pull request list shows each open stack as one expandable row
with its mode, size and a per-layer status bar, listing the layers that match the
current filters. Rows start collapsed and expand while a filter or search is active.
Selecting all rows skips layers of collapsed stacks.
Choose **Flat** to list every pull request with a stack badge instead; the choice
is remembered per user.

Append additional pull requests whose bases continue the chain. To add a layer
in the middle, branch from the layer below, open a pull request targeting that
layer's branch, and choose **Insert pull request** on the stack page. It goes
directly above the branch it targets, or at the bottom when it targets the
trunk; the layer that was there is retargeted onto it. The stack page offers
pull requests on the trunk only when they share commits with the bottom layer.
The stack keeps its number and history. In merge mode, **Update stack** then
merges the new layer into the layers above. In rebase mode, every layer above
must already contain the new layer: restack them onto it locally (for example
with `git rebase --update-refs`), push them, insert, then synchronize the stack.
The insert records a new parent boundary only for the layer directly above;
higher layers keep their old boundaries, which a server rebase would replay
from, until the synchronization.

For other restructures, unstack the stack, adjust the ordinary branches and pull
request bases, then adopt the resulting chain. Unstacking preserves branches,
pull requests and the original stack's history. Already merged entries retain
their history.

Permanently deleting a pull request dissolves its associated stack groupings and
preserves the other pull requests and branches. Deletion is refused while an
associated stack operation is active. Deleting a repository also removes its
stack records.

## Stack modes

A stack's mode is chosen at creation and cannot change; to switch, unstack and
adopt the chain again.

* **Rebase** (the API and CLI default) replays layers onto their updated parents. It keeps layer
  history linear and needs force-push permission on the layer branches.
* **Merge** (the web form default) merges each updated parent into its layer. Nothing in the stack's
  lifecycle force-pushes, so it works on long-lived, shared branches where
  force-push is disabled. Only push permission is needed.

In merge mode, layers may contain merge commits, including merges of topic
branches. Layers may fall behind their parents; updating or landing the stack
merges each parent in. Content merged in from outside the stack shows up in the
layer's diff, so bring outside branches in through the trunk.

Merge-mode stacks land with `merge`, `squash` or `fast-forward-only`. A layer lands
only once it contains the trunk head, so a squash commit carries exactly the
landed layer's content. The next update records that squash commit in the layer
above without changing its files, leaving the trunk with one commit per layer.
`rebase` landing is not available because it rewrites merge commits.

A merge-mode layer can also be updated on its own, merging its stack parent into
it, when its base is that parent and no stack operation is active. Updating by
rebase stays blocked.

## Rebase and land

Server rebase replays each layer's commits onto its updated parent using saved
parent commit boundaries. It builds candidates before publishing source refs and
uses expected-old-head leases when pushing. Source branch force-push permissions
and signing requirements still apply.

The stack page defaults to landing the whole remaining stack. Choose **Through**
a layer to land only that layer and its unmerged predecessors. When the repository
allows only one compatible merge method, the page shows its name without a selector.
Landing uses Gitea's merge, squash or rebase merge style. The worker merges one pull
request at a time, records the actual result, rebases the remaining layers and rechecks eligibility
before proceeding. It waits when required checks or reviews are outstanding.

Landing can partially complete. If two pull requests merge and the third is
blocked, those first two remain merged. Canceling an operation does not undo
published commits or merged pull requests. The operation status and journal show
which work completed and where execution stopped.

While an operation is active, structural edits are rejected. Use the stack's
controls to merge, update or retarget its members. Open stack trunk and layer
branches cannot be deleted or renamed; finish or unstack the stack first.

## Recovery

* **Checks or reviews pending:** complete the required checks or review. Existing
  operations are woken by relevant notifications; retry is also available.
* **Conflict before publication:** cancel the blocked operation, resolve and
  rebase locally, push with explicit leases, and synchronize the verified layer
  boundaries before requesting another server operation. Alternatively unstack
  the open suffix, repair its chain and adopt it again.
* **A branch changed during an operation:** the old lease remains authoritative.
  Retry does not silently accept a different head. Inspect the operation's
  journal and the live branches before repairing the chain.
* **Partial source publication:** retry reconciles each recorded old/new ref.
  Cancel also reconciles published work before releasing the operation, preserving
  later branch updates. If their ancestry cannot be verified, repair locally and
  synchronize explicit boundaries. Never replace a rejected lease with an
  unconditional force-push.
* **Interrupted merge publication:** the worker records the exact candidate before
  pushing. Recovery checks that candidate against the trunk and completes a
  missing database merge record without publishing a second merge. A changed
  source head or an unrecognized result blocks progress for inspection.
* **Signed source history required:** use a signed local rebase if the server
  cannot produce commits accepted by the source branch's signing policy.
* **Merge-mode conflict:** the operation stops with the conflicting files and the
  parent merges to repeat locally, bottom-up. Lower layers are listed too because
  their server-side merges are not published until every layer builds. Push
  normally, then retry. Retry adopts a layer head that only moved forward; any
  other change still blocks the operation.

The command-line client under `contrib/gitea-stack` provides local replay and
operation controls. Its README documents the supported commands and conflict
continuation workflow.

## REST and workflows

The repository API exposes stacks at
`/api/v1/repos/{owner}/{repo}/stacks`. Stack numbers are separate from pull request
numbers. Existing integer `pull_requests` fields identify PRs in the stack
repository. Cross-repository creation and append use `pull_request_refs` instead:

```json
{"trunk":"main","pull_request_refs":[{"repository_id":10,"pull_request":1},{"repository_id":20,"pull_request":1}]}
```

Insert accepts `pull_request_ref` with the same shape. Synchronization heads add
`repository_id` beside `pull_request`; omitting it selects the stack repository.
The caller must be able to read each referenced repository.

Mutations require an expected stack revision and return a conflict when
the stack changed or another operation owns it. Check `/stacks/capabilities`
before offering stack creation in a client.

`POST /stacks/{id}/insert` inserts one pull request by number and returns the
updated stack. Rebase, update and landing return an operation resource. `POST /stacks/{id}/update`
updates a merge-mode stack; `/rebase` applies only to rebase-mode stacks. Poll
`/stacks/{id}/operations/{operation}` for progress, and use its `retry` and
`cancel` actions for recovery. `POST /stacks/{id}/sync` explicitly records locally
published boundaries: submit every open layer's pull request number, expected
head SHA and parent SHA. The server validates the complete current chain; this
endpoint does not push branches or guess old replay boundaries.

Pull request payloads include `stack.number`, `stack.repository`, `stack.size`,
`stack.position` and `stack.base.repository`/`stack.base.ref`/`stack.base.sha`. The ordinary `base` fields continue to describe
the immediate parent. Actions branch filters use the trunk while path filters
use the layer diff. Trusted `pull_request_target` workflow selection uses the
trunk rather than an unmerged parent branch. Fork layers remain untrusted fork
inputs when workflows run in the main repository.

The generated API documentation describes request and response schemas. The
importable `modules/stackclient` package supports the stack REST workflow without
requiring changes to the external Gitea SDK.

## Scope

This implementation supports linear stacks within a repository or from one direct
fork. It does not combine source branches from several forks, change repository
visibility, provide a GraphQL server, speculative merge-group queues, atomic
whole-stack publication, or GitHub CLI/API compatibility. Stack landing follows
Gitea's existing per-pull-request merge semantics.
