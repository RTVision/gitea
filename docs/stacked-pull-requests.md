# Stacked pull requests

A stack is an ordered chain of pull requests in one repository. The first open
pull request targets the stack's trunk branch; each later pull request targets
the preceding layer's branch. The trunk can be any repository branch.

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

Create ordinary pull requests whose branches form a linear chain, then open
**Stacks** from the repository's pull request list. Select **New stack**, enter
the trunk branch and the pull request numbers in landing order.

For example, with `release` as the trunk:

```text
release ← feature/storage ← feature/api ← feature/ui
          PR 41             PR 42         PR 43
```

Each branch must contain its current parent's head, with at least one additional
commit. Layer history must be linear. Cross-repository branches, duplicate
membership, multiple open pull requests sharing a head branch, and already
scheduled ordinary auto-merges cannot be adopted.

The stack box on each pull request links the layers in landing order. Review the
individual layer diff as usual. A closed but unmerged predecessor does not satisfy
the stack's landing order.

Append additional pull requests whose bases continue the chain. To restructure
an open stack, unstack it, adjust the ordinary branches and pull request bases,
then adopt the resulting chain. Unstacking preserves branches, pull requests and
the original stack's history. Already merged entries retain their history.

Permanently deleting a pull request dissolves its associated stack groupings and
preserves the other pull requests and branches. Deletion is refused while an
associated stack operation is active. Deleting a repository also removes its
stack records.

## Rebase and land

Server rebase replays each layer's commits onto its updated parent using saved
parent commit boundaries. It builds candidates before publishing source refs and
uses expected-old-head leases when pushing. Source branch force-push permissions
and signing requirements still apply.

Landing selects the next open layer, a prefix, or the whole stack, using Gitea's
merge, squash or rebase merge style. The worker merges one pull request at a time,
records the actual result, rebases the remaining layers and rechecks eligibility
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

The command-line client under `contrib/gitea-stack` provides local replay and
operation controls. Its README documents the supported commands and conflict
continuation workflow.

## REST and workflows

The repository API exposes stacks at
`/api/v1/repos/{owner}/{repo}/stacks`. Stack numbers are separate from pull request
numbers. Mutations require an expected stack revision and return a conflict when
the stack changed or another operation owns it. Check `/stacks/capabilities`
before offering stack creation in a client.

Rebase and landing return an operation resource. Poll
`/stacks/{id}/operations/{operation}` for progress, and use its `retry` and
`cancel` actions for recovery. `POST /stacks/{id}/sync` explicitly records locally
published boundaries: submit every open layer's pull request number, expected
head SHA and parent SHA. The server validates the complete current chain; this
endpoint does not push branches or guess old replay boundaries.

Pull request payloads include `stack.number`, `stack.size`, `stack.position` and
`stack.base.ref`/`stack.base.sha`. The ordinary `base` fields continue to describe
the immediate parent. Actions branch filters use the trunk while path filters
use the layer diff. Trusted `pull_request_target` workflow selection uses the
trunk rather than an unmerged parent branch.

The generated API documentation describes request and response schemas. The
importable `modules/stackclient` package supports the stack REST workflow without
requiring changes to the external Gitea SDK.

## Scope

This implementation supports same-repository linear stacks. It does not provide
cross-fork stacks, a GraphQL server, speculative merge-group queues, atomic
whole-stack publication, or GitHub CLI/API compatibility. Stack landing follows
Gitea's existing per-pull-request merge semantics.
