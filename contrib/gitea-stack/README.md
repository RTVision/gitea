# gitea-stack

`gitea-stack` manages linear pull request stacks against Gitea's native stack API. Build it from the Gitea checkout:

```sh
go build -o gitea-stack ./contrib/gitea-stack
```

Set `GITEA_TOKEN` (or `GITEA_STACK_TOKEN`) for commands that call the API. The server URL is derived from the selected Git remote; set `GITEA_URL=https://code.example.com/gitea` when the instance uses a URL prefix or the Git remote is SSH. Choose a remote with global `--remote`, `stack.remote`, or a repository with exactly one remote.

Start a local stack with explicit ordered branches. Omitting branches creates an empty definition that `new` can extend.

```sh
gitea-stack init --trunk main --remote origin feat/api feat/ui
gitea-stack new feat/tests
gitea-stack bottom
gitea-stack up
gitea-stack checkout '#42'
```

`submit` pushes with an exact force-with-lease, creates missing PRs, and creates or appends the server stack. It does not retry revision conflicts. Existing stacked PRs with the wrong base require an explicit unstack and restructure.

```sh
gitea-stack submit --draft
gitea-stack push
gitea-stack status
gitea-stack list
gitea-stack adopt --trunk main --prs '#41,#42'
```

`sync` fetches named stack refs and refreshes server state. It never rewrites branches. It adopts a changed remote head as a new push lease only when the local branch already matches it or the server's stack snapshot records that exact head, such as after a durable landing operation rewrites the open suffix. Another user's unrecorded push still requires explicit reconciliation.

```sh
gitea-stack sync
gitea-stack restack --sign
gitea-stack restack --continue
gitea-stack push
```

Local restack uses the saved parent boundary for every layer. It snapshots heads under `refs/gitea-stack/backup/<timestamp>/`, rebases the open suffix in order, and never pushes. On a conflict, resolve files, stage them, and run `restack --continue`. Use `restack --abort` to restore all changed refs with compare-and-swap checks. `restack --status` reports saved progress. `--signing-key KEY` selects a key; `--no-sign` disables configured signing.

After a local restack, `push` publishes every open layer with its previously accepted lease and calls the stack synchronization API with the exact new heads and parent boundaries. A server revision conflict is final: refresh with `sync`, inspect the changes, and choose the next action. Backup refs remain available through `snapshots list` and `snapshots restore <timestamp> <branch>`.

Landing and server rebase use durable operations:

```sh
gitea-stack land --through 2 --merge-style squash --wait --timeout 15m
gitea-stack rebase --server
gitea-stack op list
gitea-stack op status 17
gitea-stack op wait 17
gitea-stack op cancel 17
gitea-stack op retry 17
```

`land --through` is required. Ordered landing can finish a prefix and block on a later layer; the operation's `completed` value records what already landed. Run `sync`, restack the remaining open suffix, and push it with synchronized boundaries. `unstack` removes active membership while retaining PRs, branches, and local Git backup refs.

Global `--json` emits one JSON object on stdout. Human progress and errors use stderr. Local-only commands (`init`, `new`, navigation, local `restack`, and snapshots) do not require an API token.

Exit codes are: `2` usage, `3` local precondition, `4` revision conflict, `5` restack conflict, `6` rejected remote or operation, `7` token error, `8` missing stack/PR/branch, and `9` stacks disabled. Unexpected failures use `1`.
