# gitea-stack agent guide

Use explicit selectors and inspect before mutating:

1. Run `gitea-stack status` and `gitea-stack sync` before server mutations.
2. Use `gitea-stack land --through <position>`; never infer a landing prefix.
3. Treat revision conflicts and force-with-lease failures as final. Re-run `sync`, inspect the remote change, and do not repeat the mutation automatically.
4. Run `gitea-stack restack` locally. If it exits `5`, resolve and stage conflicts, then run `restack --continue`; use `restack --abort` to restore the whole local operation.
5. Run `gitea-stack push` after a successful local restack. It publishes with saved leases and synchronizes exact server boundaries.
6. If an operation is blocked, inspect it with `op status`; use `op retry` only after resolving the reported condition. Cancellation does not roll back landed layers.

Do not edit `.git/gitea-stack/*.json`, delete `refs/gitea-stack/backup/*`, force-push outside the CLI, or guess a missing parent boundary. Use `--json` for automation and branch/PR/stack selectors exactly as printed.
