// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.dev/contrib/gitea-stack/internal/gitx"
	"gitea.dev/contrib/gitea-stack/internal/localstate"
	"gitea.dev/modules/json"
	"gitea.dev/modules/stackclient"
	api "gitea.dev/modules/structs"

	"go.yaml.in/yaml/v4"
)

type commandError struct {
	code int
	kind string
	err  error
}

func (e commandError) Error() string { return e.err.Error() }

type application struct {
	repo        gitx.Repo
	store       *localstate.Store
	jsonOutput  bool
	quiet       bool
	remoteFlag  string
	stackFlag   string
	commandName string
}

func fail(code int, kind, format string, args ...any) error {
	return commandError{code: code, kind: kind, err: fmt.Errorf(format, args...)}
}

func main() { os.Exit(runMain(os.Args[1:])) }

func runMain(arguments []string) int {
	global := flag.NewFlagSet("gitea-stack", flag.ContinueOnError)
	jsonOutput := global.Bool("json", false, "print machine-readable JSON")
	remote := global.String("remote", "", "git remote")
	stack := global.String("stack", "", "server stack id; must match the bound stack for local commands")
	quiet := global.Bool("quiet", false, "suppress progress")
	_ = global.Bool("yes", false, "accept planned updates")
	global.SetOutput(os.Stderr)
	if err := global.Parse(arguments); err != nil {
		return 2
	}
	args := global.Args()
	if len(args) == 0 {
		printUsage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	repo := gitx.Repo{Dir: ".", Ctx: ctx}
	store, err := localstate.Open(repo)
	if err != nil {
		return reportError(err, *jsonOutput)
	}
	app := &application{repo: repo, store: store, jsonOutput: *jsonOutput, quiet: *quiet, remoteFlag: *remote, stackFlag: *stack, commandName: args[0]}
	if err := app.preflightStackBinding(args[0]); err != nil {
		return reportError(err, *jsonOutput)
	}
	unlock, err := store.Lock()
	if err != nil {
		kind := "locked"
		if _, ok := errors.AsType[localstate.ForeignLockError](err); ok {
			kind = "lock_foreign_host"
		}
		return reportError(fail(3, kind, "%v", err), *jsonOutput)
	}
	defer unlock()
	if store.RestackExists() && !(args[0] == "restack" || args[0] == "snapshots") {
		return reportError(fail(3, "restack_in_progress", "restack in progress; run restack --continue or --abort"), *jsonOutput)
	}
	err = app.run(ctx, args[0], args[1:])
	if err != nil {
		return reportError(err, *jsonOutput)
	}
	return 0
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: gitea-stack [--json] [--remote name] [--stack S12] <command> [options]")
}

func reportError(err error, jsonOutput bool) int {
	if gitErr, ok := errors.AsType[gitx.ContextError](err); ok {
		err = mapGitContextError(gitErr.Operation, gitErr)
	}
	code, kind := 1, "unexpected"
	if commandErr, ok := errors.AsType[commandError](err); ok {
		code, kind = commandErr.code, commandErr.kind
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": map[string]any{"code": kind, "message": err.Error()}})
	} else {
		fmt.Fprintln(os.Stderr, "gitea-stack:", err)
	}
	return code
}

func (a *application) success(value any) {
	if a.jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "command": a.commandName, "result": value})
	}
}

func (a *application) progress(format string, args ...any) {
	if !a.quiet {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

func (a *application) run(ctx context.Context, command string, args []string) error {
	switch command {
	case "init":
		return a.init(args)
	case "new":
		return a.newLayer(args)
	case "status":
		return a.status(ctx)
	case "list":
		return a.list(ctx)
	case "up", "down", "top", "bottom":
		return a.navigate(command)
	case "checkout":
		return a.checkout(args)
	case "push":
		return a.push(ctx, args)
	case "submit":
		return a.submit(ctx, args)
	case "adopt":
		return a.adopt(ctx, args)
	case "sync":
		if len(args) != 0 {
			return fail(2, "usage", "sync takes no options")
		}
		return a.sync(ctx)
	case "restack":
		return a.restack(ctx, args)
	case "rebase":
		return a.serverRebase(ctx, args)
	case "land":
		return a.land(ctx, args)
	case "op":
		return a.operation(ctx, args)
	case "unstack":
		return a.unstack(ctx, args)
	case "capabilities":
		return a.capabilities(ctx)
	case "snapshots":
		return a.snapshots(args)
	default:
		return fail(2, "usage", "unknown command %q", command)
	}
}

func (a *application) state() (*localstate.State, error) {
	state, err := a.store.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fail(8, "not_found", "no local stack; run gitea-stack init or adopt")
		}
		return nil, err
	}
	return state, nil
}

func (a *application) optionalState() (*localstate.State, bool, error) {
	state, err := a.store.Load()
	if errors.Is(err, os.ErrNotExist) {
		return &localstate.State{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return state, true, nil
}

func isBoundStackCommand(command string) bool {
	switch command {
	case "push", "submit", "sync", "land", "unstack":
		return true
	default:
		return false
	}
}

func (a *application) preflightStackBinding(command string) error {
	if !isBoundStackCommand(command) {
		return nil
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	allowUnsubmitted := command == "push" || command == "submit"
	_, err = a.boundStackNumber(state, allowUnsubmitted)
	return err
}

func parseStack(value string) (int64, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "S")
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, fail(2, "usage", "invalid stack number %q", value)
	}
	return n, nil
}

func parsePulls(value string) ([]int64, error) {
	parts := strings.Split(value, ",")
	result := make([]int64, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(part), "#"), 10, 64)
		if err != nil || n <= 0 {
			return nil, fail(2, "usage", "invalid pull request list %q", value)
		}
		result = append(result, n)
	}
	return result, nil
}

func parseMode(value string) (api.StackMode, error) {
	switch mode := api.StackMode(value); mode {
	case "", api.StackModeRebase, api.StackModeMerge:
		return mode, nil
	default:
		return "", fail(2, "usage", "invalid --mode %q; use rebase or merge", value)
	}
}

// checkServerMode keeps a local stack from publishing with the other mode's rules, such as force-pushing merge-mode layers.
func checkServerMode(state *localstate.State, server *api.PullRequestStack) error {
	return checkLocalMode(state, server.Number, server.Mode)
}

func checkLocalMode(state *localstate.State, stack int64, serverMode api.StackMode) error {
	serverMode, localMode := cmp.Or(serverMode, api.StackModeRebase), cmp.Or(state.Mode, api.StackModeRebase)
	if serverMode != localMode {
		return fail(3, "mode_mismatch", "S%d is a %s-mode stack but the local stack is in %s mode.\nRun adopt with its open pull requests to bind S%d before publishing.", stack, serverMode, localMode, stack)
	}
	return nil
}

// checkPullStackModes refuses to publish unbound layers whose pull requests already belong to a stack of another mode.
func (a *application) checkPullStackModes(ctx context.Context, client *stackclient.Client, state *localstate.State, through int) error {
	for _, layer := range state.Layers[:through] {
		if layer.PullRequest == 0 || layer.LandedSHA != "" {
			continue
		}
		pull, err := client.GetPull(ctx, layer.PullRequest)
		if err != nil {
			return mapAPIError(err)
		}
		if pull.Stack != nil {
			if err := checkLocalMode(state, pull.Stack.Number, pull.Stack.Mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func modeMismatch(stack int64, mode api.StackMode) error {
	return fail(3, "mode_mismatch", "S%d is a %s-mode stack; a stack's mode cannot change.\nUnstack and adopt the chain again to switch modes.", stack, mode)
}

func (a *application) selectedRemote(state *localstate.State) (string, error) {
	if a.remoteFlag != "" {
		return a.remoteFlag, nil
	}
	if state != nil && state.Remote != "" {
		return state.Remote, nil
	}
	if configured, err := a.repo.Run(nil, "config", "--get", "stack.remote"); err == nil && configured != "" {
		return configured, nil
	}
	remotes, err := a.repo.Remotes()
	if err != nil {
		return "", err
	}
	if len(remotes) != 1 {
		return "", fail(2, "usage", "select a remote with --remote")
	}
	return remotes[0], nil
}

func (a *application) client(state *localstate.State) (*stackclient.Client, error) {
	remote, err := a.selectedRemote(state)
	if err != nil {
		return nil, err
	}
	remoteURL, err := a.repo.RemoteURL(remote)
	if err != nil {
		return nil, err
	}
	client, err := stackclient.FromRemote(remoteURL, cmp.Or(os.Getenv("GITEA_TOKEN"), os.Getenv("GITEA_STACK_TOKEN")))
	if err != nil {
		if _, ok := errors.AsType[stackclient.ErrAmbiguousRemoteURL](err); ok {
			return nil, fail(3, "url_ambiguous", "%v", err)
		}
		return nil, err
	}
	if client.Token == "" {
		base, _ := url.Parse(client.BaseURL) // validated by FromRemote
		client.Token = teaToken(base.Host, remoteHost(remoteURL))
		if client.Token == "" {
			return nil, fail(7, "missing_token", "set GITEA_TOKEN or add a tea login for %s", base.Host)
		}
	}
	return client, nil
}

func remoteHost(remoteURL string) string {
	if u, err := url.Parse(remoteURL); err == nil && u.Host != "" {
		return u.Hostname()
	}
	host, _, _ := strings.Cut(remoteURL[strings.LastIndex(remoteURL, "@")+1:], ":")
	return host
}

func teaToken(serverHost, sshHost string) string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	data, err := os.ReadFile(filepath.Join(dir, "tea", "config.yml"))
	if err != nil {
		return ""
	}
	var config struct {
		Logins []struct {
			URL     string `yaml:"url"`
			Token   string `yaml:"token"`
			Default bool   `yaml:"default"`
			SSHHost string `yaml:"ssh_host"`
		} `yaml:"logins"`
	}
	if yaml.Unmarshal(data, &config) != nil {
		return ""
	}
	token := ""
	for _, login := range config.Logins {
		u, err := url.Parse(login.URL)
		if login.Token == "" || !(err == nil && strings.EqualFold(u.Host, serverHost) || login.SSHHost != "" && strings.EqualFold(login.SSHHost, sshHost)) {
			continue
		}
		if login.Default {
			return login.Token
		}
		token = cmp.Or(token, login.Token)
	}
	return token
}

func mapAPIError(err error) error {
	var revision stackclient.ErrRevision
	var forbidden stackclient.ErrForbidden
	var notFound stackclient.ErrNotFound
	var disabled stackclient.ErrDisabled
	switch {
	case errors.As(err, &revision):
		return fail(4, "revision_conflict", "%v", revision)
	case errors.As(err, &forbidden):
		return fail(6, "forbidden", "%v", forbidden)
	case errors.As(err, &notFound):
		return fail(8, "not_found", "%v", notFound)
	case errors.As(err, &disabled):
		return fail(9, "disabled", "%v", disabled)
	default:
		return err
	}
}

func mapGitContextError(operation string, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fail(6, "git_timeout", "git %s timed out because its deadline expired. Check network access to the remote and retry.", operation)
	case errors.Is(err, context.Canceled):
		return fail(6, "git_timeout", "git %s was canceled. Check network access to the remote and retry.", operation)
	default:
		return fail(6, "remote_failed", "%v", err)
	}
}

func (a *application) init(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	trunk := flags.String("trunk", "", "trunk branch")
	remoteFlag := flags.String("remote", "", "git remote")
	modeValue := flags.String("mode", "", "stack mode: rebase (default) or merge")
	if err := flags.Parse(args); err != nil || *trunk == "" {
		return fail(2, "usage", "init requires --trunk and explicit ordered branches")
	}
	mode, err := parseMode(*modeValue)
	if err != nil {
		return err
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	if err := a.repo.ValidateBranch(*trunk); err != nil {
		return fail(2, "usage", "invalid trunk: %v", err)
	}
	remote := *remoteFlag
	if remote == "" {
		remote, err = a.selectedRemote(nil)
		if err != nil {
			return err
		}
	}
	parentSHA, err := a.repo.Head(*trunk)
	if err != nil {
		return fail(8, "not_found", "trunk %s: %v", *trunk, err)
	}
	state := &localstate.State{Remote: remote, Trunk: *trunk, Mode: cmp.Or(mode, api.StackModeRebase), LastSyncedTrunkSHA: parentSHA, Layers: []localstate.Layer{}}
	parent := *trunk
	for _, branch := range flags.Args() {
		if err := a.repo.ValidateBranch(branch); err != nil {
			return fail(2, "usage", "invalid branch %q", branch)
		}
		head, err := a.repo.Head(branch)
		if err != nil {
			return fail(8, "not_found", "branch %s: %v", branch, err)
		}
		if err := a.repo.IsAncestor(parentSHA, head); err != nil || head == parentSHA {
			return fail(3, "invalid_chain", "%s is not a non-empty descendant of %s", branch, parent)
		}
		remoteSHA, _ := a.repo.Head("refs/remotes/" + remote + "/" + branch)
		state.Layers = append(state.Layers, localstate.Layer{Branch: branch, HeadSHA: head, ParentSHA: parentSHA, RemoteSHA: remoteSHA})
		parent, parentSHA = branch, head
	}
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "initialized %d layers on %s\n", len(state.Layers), state.Trunk)
	}
	a.success(state)
	return nil
}

func (a *application) newLayer(args []string) error {
	flags := flag.NewFlagSet("new", flag.ContinueOnError)
	from := flags.String("from", "", "parent branch")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		return fail(2, "usage", "new requires one branch name")
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	branch := flags.Arg(0)
	if err := a.repo.ValidateBranch(branch); err != nil {
		return fail(2, "usage", "invalid branch: %v", err)
	}
	parent := *from
	if parent == "" {
		parent = state.Trunk
		if len(state.Layers) != 0 {
			parent = state.Layers[len(state.Layers)-1].Branch
		}
	}
	if len(state.Layers) != 0 && parent != state.Layers[len(state.Layers)-1].Branch {
		return fail(3, "invalid_chain", "new layers must extend the current stack top %s", state.Layers[len(state.Layers)-1].Branch)
	}
	if len(state.Layers) == 0 && parent != state.Trunk {
		return fail(3, "invalid_chain", "the first layer must start from trunk %s", state.Trunk)
	}
	parentSHA, err := a.repo.Head(parent)
	if err != nil {
		return err
	}
	if err := a.repo.SwitchCreate(branch, parent); err != nil {
		return err
	}
	state.Layers = append(state.Layers, localstate.Layer{Branch: branch, HeadSHA: parentSHA, ParentSHA: parentSHA})
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintln(os.Stdout, branch)
	}
	a.success(state.Layers[len(state.Layers)-1])
	return nil
}

func (a *application) serverStackNumber(state *localstate.State) (int64, error) {
	if a.stackFlag != "" {
		return parseStack(a.stackFlag)
	}
	if state == nil || state.Stack == 0 {
		return 0, fail(8, "not_found", "local stack has not been submitted")
	}
	return state.Stack, nil
}

func (a *application) boundStackNumber(state *localstate.State, allowUnsubmitted bool) (int64, error) {
	if state.Stack == 0 {
		if allowUnsubmitted && a.stackFlag == "" {
			return 0, nil
		}
		return 0, fail(3, "stack_unsubmitted", "this stack has not been submitted yet.\nRun `submit` first, then retry.")
	}
	if a.stackFlag == "" {
		return state.Stack, nil
	}
	number, err := parseStack(a.stackFlag)
	if err != nil {
		return 0, err
	}
	if number != state.Stack {
		return 0, fail(3, "stack_mismatch", "--stack S%d does not match the stack bound to this branch (S%d).\nRun the command without --stack, or check out a branch on S%d.", number, state.Stack, number)
	}
	return number, nil
}

func (a *application) status(ctx context.Context) error {
	state, hasState, err := a.optionalState()
	if err != nil {
		return err
	}
	if !hasState && a.stackFlag == "" {
		return fail(8, "not_found", "no local stack; run gitea-stack init or adopt")
	}
	number := int64(0)
	if state == nil || state.Stack != 0 || a.stackFlag != "" {
		number, err = a.serverStackNumber(state)
		if err != nil {
			return err
		}
	}
	var server *api.PullRequestStack
	if number != 0 {
		result, err := a.statusServer(ctx, state, number)
		if err != nil {
			return err
		}
		server = result.Stack
	}
	local := state
	if a.stackFlag != "" || (local != nil && local.Stack != number) {
		local = nil
	}
	if !a.jsonOutput {
		if local == nil {
			fmt.Fprintf(os.Stdout, "S%d on %s  rev %d  op %d\n", server.Number, server.Trunk, server.Revision, server.ActiveOperation)
			for _, entry := range server.Entries {
				pull := int64(0)
				branch := ""
				if entry.PullRequest != nil {
					pull = entry.PullRequest.Index
					if entry.PullRequest.Head != nil {
						branch = entry.PullRequest.Head.Ref
					}
				}
				status := "open"
				if entry.LandedSHA != "" {
					status = "merged " + short(entry.LandedSHA)
				}
				fmt.Fprintf(os.Stdout, " %d  %s  #%d  %s\n", entry.Position, branch, pull, status)
			}
			a.success(map[string]any{"local": nil, "server": server})
			return nil
		}
		fmt.Fprintln(os.Stdout, statusHeader(local, server))
		for i, layer := range local.Layers {
			status := "open"
			if layer.LandedSHA != "" {
				status = "merged " + short(layer.LandedSHA)
			}
			fmt.Fprintf(os.Stdout, " %d  %s  #%d  %s\n", i+1, layer.Branch, layer.PullRequest, status)
		}
	}
	a.success(map[string]any{"local": local, "server": server})
	return nil
}

func statusHeader(local *localstate.State, server *api.PullRequestStack) string {
	if server == nil {
		return fmt.Sprintf("S%d on %s rev %d (server unavailable)", local.Stack, local.Trunk, local.LastRevision)
	}
	return fmt.Sprintf("S%d on %s rev %d op %d", local.Stack, local.Trunk, local.LastRevision, server.ActiveOperation)
}

type statusServerResult struct {
	Stack *api.PullRequestStack
}

func (a *application) statusServer(ctx context.Context, state *localstate.State, number int64) (statusServerResult, error) {
	client, err := a.client(state)
	if err != nil {
		if a.stackFlag != "" {
			return statusServerResult{}, err
		}
		return statusServerResult{}, nil
	}
	server, err := client.GetStack(ctx, number)
	if err != nil {
		if a.stackFlag != "" {
			return statusServerResult{}, mapAPIError(err)
		}
		return statusServerResult{}, nil
	}
	return statusServerResult{Stack: server}, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (a *application) list(ctx context.Context) error {
	client, err := a.client(nil)
	if err != nil {
		return err
	}
	stacks, err := client.ListStacks(ctx, 1, 50)
	if err != nil {
		return mapAPIError(err)
	}
	if !a.jsonOutput {
		for _, stack := range stacks {
			fmt.Fprintf(os.Stdout, "S%d  %s  rev %d  %d layers  %s\n", stack.Number, stack.Trunk, stack.Revision, len(stack.Entries), stack.State)
		}
	}
	a.success(stacks)
	return nil
}

func layerIndex(state *localstate.State, selector string) (int, error) {
	if position, err := strconv.Atoi(selector); err == nil && position >= 1 && position <= len(state.Layers) {
		return position - 1, nil
	}
	if value, ok := strings.CutPrefix(selector, "#"); ok {
		pr, err := strconv.ParseInt(value, 10, 64)
		if err == nil {
			for i := range state.Layers {
				if state.Layers[i].PullRequest == pr {
					return i, nil
				}
			}
		}
	}
	for i := range state.Layers {
		if state.Layers[i].Branch == selector {
			return i, nil
		}
	}
	return 0, fail(8, "not_found", "layer %q is not in this stack", selector)
}

func (a *application) navigate(direction string) error {
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	current, err := a.repo.CurrentBranch()
	if err != nil {
		return fail(3, "precondition", "detached HEAD is not a stack layer")
	}
	index, err := layerIndex(state, current)
	if err != nil {
		return err
	}
	switch direction {
	case "up":
		index++
	case "down":
		index--
	case "top":
		index = len(state.Layers) - 1
	case "bottom":
		index = 0
	}
	if index < 0 || index >= len(state.Layers) {
		return fail(3, "precondition", "already at the %s of the stack", direction)
	}
	return a.switchLayer(state.Layers[index].Branch)
}

func (a *application) checkout(args []string) error {
	if len(args) != 1 {
		return fail(2, "usage", "checkout requires a branch, #PR, or position")
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	index, err := layerIndex(state, args[0])
	if err != nil {
		return err
	}
	return a.switchLayer(state.Layers[index].Branch)
}

func (a *application) switchLayer(branch string) error {
	if err := a.repo.Switch(branch); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintln(os.Stdout, branch)
	}
	a.success(map[string]string{"branch": branch})
	return nil
}

func throughIndex(state *localstate.State, value string) (int, error) {
	if value == "" {
		return len(state.Layers), nil
	}
	index, err := layerIndex(state, value)
	return index + 1, err
}

func (a *application) pushLayers(ctx context.Context, state *localstate.State, through int, beforePublish func() error) error {
	if state.Mode == api.StackModeMerge {
		return a.pushMergeLayers(ctx, state, through, beforePublish)
	}
	for i := range through {
		layer := &state.Layers[i]
		if layer.LandedSHA != "" {
			continue
		}
		head, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		remoteHead, err := a.repo.RemoteHeadContext(ctx, state.Remote, layer.Branch)
		if err != nil {
			return mapGitContextError("ls-remote", err)
		}
		if layer.RemoteSHA == "" && remoteHead != "" {
			return fail(6, "lease_rejected", "remote branch %s exists without a recorded lease; run sync or adopt", layer.Branch)
		}
		if layer.RemoteSHA != "" && remoteHead != layer.RemoteSHA {
			return fail(6, "lease_rejected", "remote branch %s moved from %s to %s", layer.Branch, short(layer.RemoteSHA), short(remoteHead))
		}
		if remoteHead == head {
			continue
		}
		a.progress("pushing %s with lease %s", layer.Branch, short(layer.RemoteSHA))
		if err := a.repo.PushLeaseContext(ctx, state.Remote, layer.Branch, layer.RemoteSHA); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("push", err)
			}
			return fail(6, "lease_rejected", "%s: %v", layer.Branch, err)
		}
		layer.HeadSHA, layer.RemoteSHA = head, head
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	return nil
}

// mergeLayerParents maps each open layer to the parent head it contains, which merge-mode sync records instead of a replay boundary.
func (a *application) mergeLayerParents(state *localstate.State) (map[int]string, error) {
	parent, err := a.repo.Head("refs/remotes/" + state.Remote + "/" + state.Trunk)
	if err != nil {
		return nil, fail(3, "precondition", "fetch the trunk with sync before pushing: %v", err)
	}
	parents := make(map[int]string, len(state.Layers))
	for i, layer := range state.Layers {
		if layer.LandedSHA != "" {
			continue
		}
		head, err := a.repo.Head(layer.Branch)
		if err != nil {
			return nil, err
		}
		if a.repo.IsAncestor(parent, head) != nil {
			return nil, fail(3, "precondition", "%s does not contain its parent head %s; run restack before pushing", layer.Branch, short(parent))
		}
		parents[i], parent = parent, head
	}
	return parents, nil
}

// pushMergeLayers publishes fast-forwards only, checking every layer before pushing any and leasing each on the checked remote head.
func (a *application) pushMergeLayers(ctx context.Context, state *localstate.State, through int, beforePublish func() error) error {
	type update struct {
		layer            *localstate.Layer
		head, remoteHead string
	}
	updates := make([]update, 0, through)
	for i := range through {
		layer := &state.Layers[i]
		if layer.LandedSHA != "" {
			continue
		}
		head, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		remoteHead, err := a.repo.RemoteHeadContext(ctx, state.Remote, layer.Branch)
		if err != nil {
			return mapGitContextError("ls-remote", err)
		}
		if remoteHead == head {
			continue
		}
		if remoteHead != "" && a.repo.IsAncestor(remoteHead, head) != nil {
			return fail(6, "non_fast_forward", "remote branch %s has commits missing locally (%s); run sync and merge them before pushing", layer.Branch, short(remoteHead))
		}
		updates = append(updates, update{layer: layer, head: head, remoteHead: remoteHead})
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return err
		}
	}
	for _, update := range updates {
		a.progress("pushing %s", update.layer.Branch)
		if err := a.repo.PushLeaseContext(ctx, state.Remote, update.layer.Branch, update.remoteHead); err != nil { // a plain push would re-advance a remote rewound after the check
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("push", err)
			}
			return fail(6, "lease_rejected", "%s: %v", update.layer.Branch, err)
		}
		update.layer.HeadSHA, update.layer.RemoteSHA = update.head, update.head
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) push(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("push", flag.ContinueOnError)
	throughValue := flags.String("through", "", "last layer to push")
	if err := flags.Parse(args); err != nil {
		return fail(2, "usage", "%v", err)
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	stackNumber, err := a.boundStackNumber(state, true)
	if err != nil {
		return err
	}
	through, err := throughIndex(state, *throughValue)
	if err != nil {
		return err
	}
	var client *stackclient.Client
	var server *api.PullRequestStack
	var mergeParents map[int]string
	var checkParents func() error
	if stackNumber != 0 {
		client, err = a.client(state)
		if err != nil {
			return err
		}
		server, err = client.GetStack(ctx, stackNumber)
		if err != nil {
			return mapAPIError(err)
		}
		if server.State != "open" {
			return fail(3, "precondition", "S%d is %s; push requires an open stack", server.Number, server.State)
		}
		if err := checkServerMode(state, server); err != nil {
			return err
		}
		if server.ActiveOperation != 0 {
			return fail(6, "operation_active", "stack has active operation %d", server.ActiveOperation)
		}
		for i := through; i < len(state.Layers); i++ {
			if state.Layers[i].LandedSHA == "" {
				return fail(3, "precondition", "a submitted stack push must include every open layer so server boundaries stay complete")
			}
		}
		if state.Mode == api.StackModeMerge {
			checkParents = func() (err error) {
				mergeParents, err = a.mergeLayerParents(state)
				return err
			}
		}
	} else if slices.ContainsFunc(state.Layers[:through], func(layer localstate.Layer) bool { return layer.PullRequest != 0 }) {
		unbound, err := a.client(state)
		if err != nil {
			return err
		}
		if err := a.checkPullStackModes(ctx, unbound, state, through); err != nil {
			return err
		}
	}
	if err := a.pushLayers(ctx, state, through, checkParents); err != nil {
		return err
	}
	if client != nil {
		heads := make([]api.PullRequestStackHead, 0, len(state.Layers))
		for i := range state.Layers {
			layer := &state.Layers[i]
			if layer.LandedSHA != "" {
				continue
			}
			if layer.PullRequest == 0 {
				return fail(3, "precondition", "layer %s has no pull request; run submit", layer.Branch)
			}
			head, err := a.repo.Head(layer.Branch)
			if err != nil {
				return err
			}
			if parent, ok := mergeParents[i]; ok {
				layer.ParentSHA = parent
			}
			heads = append(heads, api.PullRequestStackHead{PullRequest: layer.PullRequest, HeadSHA: head, ParentSHA: layer.ParentSHA})
		}
		server, err = client.SynchronizeStack(ctx, stackNumber, server.Revision, heads)
		if err != nil {
			return mapAPIError(err)
		}
		state.LastRevision = server.Revision
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "pushed %d layers\n", through)
	}
	a.success(state)
	return nil
}

func (a *application) submit(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("submit", flag.ContinueOnError)
	throughValue := flags.String("through", "", "last layer to submit")
	draft := flags.Bool("draft", false, "create draft pull requests")
	_ = flags.Bool("title-from-commit", true, "use the first commit subject")
	modeValue := flags.String("mode", "", "stack mode for a new server stack: rebase (default) or merge")
	if err := flags.Parse(args); err != nil {
		return fail(2, "usage", "%v", err)
	}
	mode, err := parseMode(*modeValue)
	if err != nil {
		return err
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	stackNumber, err := a.boundStackNumber(state, true)
	if err != nil {
		return err
	}
	if current := cmp.Or(state.Mode, api.StackModeRebase); mode != "" && mode != current {
		if stackNumber != 0 {
			return modeMismatch(stackNumber, current)
		}
		state.Mode = mode
	}
	through, err := throughIndex(state, *throughValue)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	if _, err := client.Capabilities(ctx); err != nil {
		return mapAPIError(err)
	}
	if stackNumber != 0 {
		server, err := client.GetStack(ctx, stackNumber)
		if err != nil {
			return mapAPIError(err)
		}
		if err := validateSubmitStack(state, server); err != nil {
			return err
		}
	} else if err := a.checkPullStackModes(ctx, client, state, through); err != nil {
		return err
	}
	if err := a.pushLayers(ctx, state, through, nil); err != nil {
		return err
	}
	for i := range through {
		layer := &state.Layers[i]
		parent := state.Trunk
		if i > 0 {
			parent = state.Layers[i-1].Branch
		}
		if layer.PullRequest != 0 {
			pull, err := client.GetPull(ctx, layer.PullRequest)
			if err != nil {
				return mapAPIError(err)
			}
			if pull.Base.Ref != parent {
				return fail(3, "restructure_required", "#%d targets %s; unstack and explicitly restructure before changing it to %s", layer.PullRequest, pull.Base.Ref, parent)
			}
			continue
		}
		title, err := a.repo.Run(nil, "log", "-1", "--format=%s", parent+".."+layer.Branch)
		if err != nil || title == "" {
			title = layer.Branch
		}
		if *draft {
			title = "WIP: " + title
		}
		body, _ := a.repo.Run(nil, "log", "-1", "--format=%b", parent+".."+layer.Branch)
		pull, err := client.CreatePull(ctx, api.CreatePullRequestOption{Head: layer.Branch, Base: parent, Title: title, Body: body})
		if err != nil {
			return mapAPIError(err)
		}
		layer.PullRequest = pull.Index
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	if stackNumber == 0 {
		pulls := make([]int64, 0, through)
		for i := range through {
			pulls = append(pulls, state.Layers[i].PullRequest)
		}
		server, err := client.CreateStack(ctx, state.Trunk, cmp.Or(state.Mode, api.StackModeRebase), pulls)
		if err != nil {
			return mapAPIError(err)
		}
		state.Stack, state.LastRevision = server.Number, server.Revision
	} else {
		server, err := client.GetStack(ctx, stackNumber)
		if err != nil {
			return mapAPIError(err)
		}
		missingPulls, err := missingSubmitPulls(state, through, server)
		if err != nil {
			return err
		}
		if len(missingPulls) != 0 {
			server, err = client.AppendStack(ctx, stackNumber, server.Revision, missingPulls)
			if err != nil {
				return mapAPIError(err)
			}
		}
		state.LastRevision = server.Revision
	}
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "S%d\n", state.Stack)
	}
	a.success(state)
	return nil
}

func validateSubmitStack(state *localstate.State, server *api.PullRequestStack) error {
	drift := func(format string, args ...any) error {
		return fail(3, "stack_drift", "S%d %s; unstack and restructure explicitly", server.Number, fmt.Sprintf(format, args...))
	}
	if server.State != "open" {
		return fail(3, "precondition", "S%d is %s; submit requires an open stack", server.Number, server.State)
	}
	if err := checkServerMode(state, server); err != nil {
		return err
	}
	if server.ActiveOperation != 0 {
		return fail(6, "operation_active", "S%d has an operation in progress; retry when it completes", server.Number)
	}
	if server.Trunk != state.Trunk {
		return drift("trunk is %s but local stack targets %s", server.Trunk, state.Trunk)
	}
	if len(server.Entries) > len(state.Layers) {
		return drift("has %d entries but local stack has %d", len(server.Entries), len(state.Layers))
	}
	localPulls := make(map[int64]struct{}, len(state.Layers))
	missingSeen := false
	for i, layer := range state.Layers {
		if layer.PullRequest == 0 {
			missingSeen = true
			continue
		}
		if missingSeen {
			return drift("has local pull request #%d after an unsubmitted layer at position %d", layer.PullRequest, i+1)
		}
		if _, duplicate := localPulls[layer.PullRequest]; duplicate {
			return drift("has duplicate local pull request #%d", layer.PullRequest)
		}
		localPulls[layer.PullRequest] = struct{}{}
	}
	serverPulls := make(map[int64]struct{}, len(server.Entries))
	for i, entry := range server.Entries {
		if entry == nil || entry.PullRequest == nil || entry.Position != i+1 {
			return drift("has malformed membership at position %d", i+1)
		}
		pull := entry.PullRequest.Index
		if pull <= 0 {
			return drift("has malformed membership at position %d", i+1)
		}
		if _, duplicate := serverPulls[pull]; duplicate {
			return drift("contains duplicate pull request #%d", pull)
		}
		serverPulls[pull] = struct{}{}
		localPull := state.Layers[i].PullRequest
		if localPull == 0 {
			return drift("has #%d at layer %d but local layer is unsubmitted", pull, i+1)
		}
		if localPull != pull {
			return drift("has #%d at layer %d but local layer is #%d", pull, i+1, localPull)
		}
	}
	return nil
}

func missingSubmitPulls(state *localstate.State, through int, server *api.PullRequestStack) ([]int64, error) {
	if err := validateSubmitStack(state, server); err != nil {
		return nil, err
	}
	if len(server.Entries) >= through {
		return nil, nil
	}
	missing := make([]int64, 0, through-len(server.Entries))
	for i := len(server.Entries); i < through; i++ {
		if state.Layers[i].PullRequest == 0 {
			return nil, fail(3, "stack_drift", "S%d has no pull request for local layer %d; unstack and restructure explicitly", server.Number, i+1)
		}
		missing = append(missing, state.Layers[i].PullRequest)
	}
	return missing, nil
}

func (a *application) adopt(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("adopt", flag.ContinueOnError)
	pullValue := flags.String("prs", "", "ordered pull request numbers")
	trunk := flags.String("trunk", "", "trunk branch")
	modeValue := flags.String("mode", "", "stack mode for a new server stack: rebase (default) or merge")
	if err := flags.Parse(args); err != nil || *pullValue == "" || *trunk == "" {
		return fail(2, "usage", "adopt requires --prs and --trunk")
	}
	mode, err := parseMode(*modeValue)
	if err != nil {
		return err
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	pulls, err := parsePulls(*pullValue)
	if err != nil {
		return err
	}
	remote, err := a.selectedRemote(nil)
	if err != nil {
		return err
	}
	temporary := &localstate.State{Remote: remote, Trunk: *trunk}
	client, err := a.client(temporary)
	if err != nil {
		return err
	}
	branches := []string{*trunk}
	apiPulls := make([]*api.PullRequest, 0, len(pulls))
	parent := *trunk
	existing := int64(-1)
	for _, number := range pulls {
		pull, err := client.GetPull(ctx, number)
		if err != nil {
			return mapAPIError(err)
		}
		if pull.Base.Ref != parent || pull.Head == nil || pull.Head.Ref == "" {
			return fail(3, "invalid_chain", "#%d does not target %s", number, parent)
		}
		stack := int64(0)
		if pull.Stack != nil {
			stack = pull.Stack.Number
		}
		if existing >= 0 && stack != existing {
			return fail(3, "invalid_chain", "#%d is not in the same stack as #%d", number, pulls[0])
		}
		existing = stack
		apiPulls = append(apiPulls, pull)
		branches = append(branches, pull.Head.Ref)
		parent = pull.Head.Ref
	}
	var landed []localstate.Layer
	var server *api.PullRequestStack
	if existing > 0 {
		server, err = client.GetStack(ctx, existing)
		if err != nil {
			return mapAPIError(err)
		}
		if landed, err = boundLandedLayers(server, *trunk, pulls); err != nil {
			return err
		}
		if current := cmp.Or(server.Mode, api.StackModeRebase); mode != "" && mode != current {
			return modeMismatch(server.Number, current)
		}
		mode = server.Mode
	}
	if err := a.repo.FetchContext(ctx, remote, branches); err != nil {
		return mapGitContextError("fetch", err)
	}
	trunkSHA, err := a.repo.Head("refs/remotes/" + remote + "/" + *trunk)
	if err != nil {
		return err
	}
	state := &localstate.State{Remote: remote, Trunk: *trunk, Mode: cmp.Or(mode, api.StackModeRebase), LastSyncedTrunkSHA: trunkSHA, Layers: landed}
	for _, pull := range apiPulls {
		remoteHead, err := a.repo.Head("refs/remotes/" + remote + "/" + pull.Head.Ref)
		if err != nil {
			return err
		}
		localHead, err := a.repo.Head(pull.Head.Ref)
		if err != nil {
			if err := a.repo.UpdateRef("refs/heads/"+pull.Head.Ref, remoteHead, ""); err != nil {
				return err
			}
			localHead = remoteHead
		}
		parentSHA := pull.Base.Sha
		if state.Mode == api.StackModeMerge && parentSHA != "" && a.repo.IsAncestor(parentSHA, localHead) != nil {
			parentSHA, _ = a.repo.Run(nil, "merge-base", parentSHA, localHead) // a merge-mode layer may be behind; restack merges the parent in
		}
		if parentSHA == "" || a.repo.IsAncestor(parentSHA, localHead) != nil {
			return fail(3, "boundary_invalid", "#%d has no verifiable saved parent boundary", pull.Index)
		}
		state.Layers = append(state.Layers, localstate.Layer{Branch: pull.Head.Ref, PullRequest: pull.Index, HeadSHA: localHead, RemoteSHA: remoteHead, ParentSHA: parentSHA})
	}
	if server == nil {
		if server, err = client.CreateStack(ctx, *trunk, state.Mode, pulls); err != nil {
			return mapAPIError(err)
		}
	}
	state.Stack, state.LastRevision = server.Number, server.Revision
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "adopted S%d\n", state.Stack)
	}
	a.success(state)
	return nil
}

// boundLandedLayers checks that pulls are the open suffix of server and returns its landed prefix, keeping local positions equal to server positions.
func boundLandedLayers(server *api.PullRequestStack, trunk string, pulls []int64) ([]localstate.Layer, error) {
	landed := make([]localstate.Layer, 0, len(server.Entries))
	open := make([]int64, 0, len(server.Entries))
	for _, entry := range server.Entries {
		if entry == nil || entry.PullRequest == nil {
			return nil, fail(3, "stack_drift", "S%d has malformed membership", server.Number)
		}
		if entry.LandedSHA == "" {
			open = append(open, entry.PullRequest.Index)
			continue
		}
		if len(open) != 0 {
			return nil, fail(3, "stack_drift", "S%d has a landed layer above an open one", server.Number)
		}
		branch := ""
		if entry.PullRequest.Head != nil {
			branch = entry.PullRequest.Head.Ref
		}
		landed = append(landed, localstate.Layer{Branch: branch, PullRequest: entry.PullRequest.Index, HeadSHA: entry.HeadSHA, ParentSHA: entry.ParentSHA, LandedSHA: entry.LandedSHA})
	}
	if server.State != "open" || server.Trunk != trunk || !slices.Equal(open, pulls) {
		return nil, fail(3, "stack_drift", "the pull requests belong to S%d; adopt must list its open layers %v in order on trunk %s", server.Number, open, server.Trunk)
	}
	return landed, nil
}

func (a *application) sync(ctx context.Context) error {
	state, err := a.state()
	if err != nil {
		return err
	}
	number, err := a.boundStackNumber(state, false)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	server, err := client.GetStack(ctx, number)
	if err != nil {
		return mapAPIError(err)
	}
	if server.State != "open" && server.State != "complete" { // a complete stack still syncs so the final landings are recorded
		return fail(3, "precondition", "S%d is %s; sync requires an open or complete stack", server.Number, server.State)
	}
	if err := checkServerMode(state, server); err != nil {
		return err
	}
	landedPulls := make(map[int64]bool, len(server.Entries))
	for _, entry := range server.Entries {
		if entry.PullRequest != nil && entry.LandedSHA != "" {
			landedPulls[entry.PullRequest.Index] = true
		}
	}
	branches := []string{state.Trunk}
	for _, layer := range state.Layers {
		if layer.LandedSHA == "" && !landedPulls[layer.PullRequest] {
			branches = append(branches, layer.Branch)
		}
	}
	if err := a.repo.FetchContext(ctx, state.Remote, branches); err != nil {
		return mapGitContextError("fetch", err)
	}
	trunkSHA, err := a.repo.Head("refs/remotes/" + state.Remote + "/" + state.Trunk)
	if err != nil {
		return err
	}
	report, err := a.updateSyncState(state, server, trunkSHA)
	if err != nil {
		return err
	}
	state.LastRevision, state.LastSyncedTrunkSHA = server.Revision, trunkSHA
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "S%d rev %d; restack: %s\n", number, server.Revision, strings.Join(report.NeedsRestack, ", "))
		if len(report.NeedsReconciliation) != 0 && state.Mode == api.StackModeMerge {
			fmt.Fprintf(os.Stdout, "Merge remote changes before pushing: %s\n", strings.Join(report.NeedsReconciliation, ", "))
		} else if len(report.NeedsReconciliation) != 0 {
			fmt.Fprintf(os.Stdout, "Reconcile remote changes before pushing: %s; previous leases retained\n", strings.Join(report.NeedsReconciliation, ", "))
		}
		if len(report.BehindElsewhere) != 0 {
			fmt.Fprintf(os.Stdout, "Behind the remote but checked out in another worktree; pull there: %s\n", strings.Join(report.BehindElsewhere, ", "))
		}
	}
	result := map[string]any{"stack": state, "needs_restack": report.NeedsRestack, "needs_reconciliation": report.NeedsReconciliation}
	if len(report.BehindElsewhere) != 0 {
		result["behind_in_other_worktree"] = report.BehindElsewhere
	}
	a.success(result)
	return nil
}

type syncReport struct {
	NeedsRestack        []string
	NeedsReconciliation []string
	BehindElsewhere     []string
}

func (a *application) updateSyncState(state *localstate.State, server *api.PullRequestStack, trunkSHA string) (*syncReport, error) {
	report := &syncReport{NeedsRestack: make([]string, 0), NeedsReconciliation: make([]string, 0)}
	entriesByPull := make(map[int64]*api.PullRequestStackEntry, len(server.Entries))
	for _, entry := range server.Entries {
		if entry.PullRequest != nil {
			entriesByPull[entry.PullRequest.Index] = entry
		}
	}
	merge := state.Mode == api.StackModeMerge
	current, _ := a.repo.CurrentBranch()
	openParent := trunkSHA
	for i := range state.Layers {
		layer := &state.Layers[i]
		entry := entriesByPull[layer.PullRequest]
		if entry != nil {
			layer.LandedSHA = entry.LandedSHA
			if merge && entry.LandedSHA != "" && entry.HeadSHA != "" {
				layer.HeadSHA = entry.HeadSHA // restack compares a squash against the head that actually landed
			}
		}
		if layer.LandedSHA != "" {
			openParent = trunkSHA
			continue
		}
		localHead, err := a.repo.Head(layer.Branch)
		if err != nil {
			return nil, err
		}
		remoteSHA, err := a.repo.Head("refs/remotes/" + state.Remote + "/" + layer.Branch)
		if merge {
			if err != nil {
				remoteSHA = ""
			}
			if localHead, err = a.syncMergeLayer(layer, current, localHead, remoteSHA, report); err != nil {
				return nil, err
			}
			if a.repo.IsAncestor(openParent, localHead) != nil {
				report.NeedsRestack = append(report.NeedsRestack, layer.Branch)
			}
			openParent = localHead
			continue
		}
		if err == nil && remoteLeaseCanAdvance(a.repo, localHead, layer.RemoteSHA, remoteSHA, entry) {
			layer.RemoteSHA = remoteSHA
		}
		if remoteSHA != "" && remoteSHA != layer.RemoteSHA {
			report.NeedsReconciliation = append(report.NeedsReconciliation, layer.Branch)
		}
		if layer.ParentSHA != openParent {
			report.NeedsRestack = append(report.NeedsRestack, layer.Branch)
		}
		openParent = localHead
	}
	return report, nil
}

// syncMergeLayer fast-forwards a local layer to its remote head and reports what it cannot fast-forward without rewriting.
func (a *application) syncMergeLayer(layer *localstate.Layer, current, localHead, remoteSHA string, report *syncReport) (string, error) {
	switch {
	case remoteSHA == "":
		return localHead, nil
	case a.repo.IsAncestor(remoteSHA, localHead) == nil:
		layer.RemoteSHA = remoteSHA
		return localHead, nil
	case a.repo.IsAncestor(localHead, remoteSHA) != nil:
		report.NeedsReconciliation = append(report.NeedsReconciliation, layer.Branch)
		return localHead, nil
	}
	worktrees, err := a.repo.WorktreesForBranch(layer.Branch)
	if err != nil {
		return "", err
	}
	switch {
	case layer.Branch == current:
		_, err = a.repo.Run(nil, "merge", "--ff-only", "--end-of-options", remoteSHA)
	case len(worktrees) != 0:
		report.BehindElsewhere = append(report.BehindElsewhere, layer.Branch)
		return localHead, nil
	default:
		err = a.repo.UpdateRef("refs/heads/"+layer.Branch, remoteSHA, localHead)
	}
	if err != nil {
		return "", fail(3, "precondition", "fast-forward %s: %v", layer.Branch, err)
	}
	layer.HeadSHA, layer.RemoteSHA = remoteSHA, remoteSHA
	return remoteSHA, nil
}

func remoteLeaseCanAdvance(repo gitx.Repo, localHead, acceptedHead, remoteHead string, entry *api.PullRequestStackEntry) bool {
	if remoteHead == "" {
		return false
	}
	if remoteHead == localHead {
		return true
	}
	if acceptedHead == "" || entry == nil || remoteHead != entry.HeadSHA {
		return false
	}
	acceptedTree, err := repo.Run(nil, "rev-parse", "--verify", "--end-of-options", acceptedHead+"^{tree}")
	if err != nil {
		return false
	}
	remoteTree, err := repo.Run(nil, "rev-parse", "--verify", "--end-of-options", remoteHead+"^{tree}")
	return err == nil && acceptedTree != "" && acceptedTree == remoteTree
}

func (a *application) restack(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("restack", flag.ContinueOnError)
	continueFlag := flags.Bool("continue", false, "continue a conflicted restack")
	abortFlag := flags.Bool("abort", false, "abort and restore all layers")
	statusFlag := flags.Bool("status", false, "show restack state")
	onto := flags.String("onto", "", "new trunk commit")
	sign := flags.Bool("sign", false, "sign rebased commits")
	signingKey := flags.String("signing-key", "", "signing key")
	noSign := flags.Bool("no-sign", false, "disable commit signing")
	if err := flags.Parse(args); err != nil {
		return fail(2, "usage", "%v", err)
	}
	if *statusFlag {
		progress, err := a.store.LoadRestack()
		if err != nil {
			return fail(8, "not_found", "no restack in progress")
		}
		if !a.jsonOutput {
			fmt.Fprintf(os.Stdout, "%s layer %d/%d\n", progress.Phase, progress.Current+1, len(progress.Layers))
		}
		a.success(progress)
		return nil
	}
	if *abortFlag {
		return a.abortRestack(ctx)
	}
	if *continueFlag {
		return a.continueRestack(ctx)
	}
	if a.store.RestackExists() {
		return fail(3, "restack_in_progress", "restack already in progress")
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	original, err := a.repo.CurrentBranch()
	if err != nil {
		return fail(3, "precondition", "restack requires an attached worktree")
	}
	newTrunk := *onto
	if newTrunk == "" {
		newTrunk, err = a.repo.Head("refs/remotes/" + state.Remote + "/" + state.Trunk)
		if err != nil {
			return fail(3, "precondition", "fetch the trunk with sync before restacking: %v", err)
		}
	} else if newTrunk, err = a.repo.Head(newTrunk); err != nil {
		return fail(8, "not_found", "--onto: %v", err)
	}
	signValue := ""
	if !*noSign {
		if *signingKey != "" {
			signValue = *signingKey
		} else if *sign {
			signValue = "default"
		} else if configured, _ := a.repo.Run(nil, "config", "--bool", "--get", "commit.gpgsign"); configured == "true" {
			signValue, _ = a.repo.Run(nil, "config", "--get", "user.signingkey")
			if signValue == "" {
				signValue = "default"
			}
		}
	}
	snapshot := fmt.Sprintf("refs/gitea-stack/backup/%d", time.Now().Unix())
	progress := &localstate.Restack{Phase: "planning", Stack: state.Stack, Trunk: state.Trunk, Mode: state.Mode, Sign: signValue, Snapshot: snapshot, OriginalBranch: original}
	newBase := newTrunk
	for _, layer := range state.Layers {
		if layer.LandedSHA != "" {
			continue
		}
		if layer.ParentSHA == "" {
			return fail(3, "boundary_unknown", "layer %s has no saved parent boundary; repair or adopt it explicitly", layer.Branch)
		}
		head, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		if err := a.repo.IsAncestor(layer.ParentSHA, head); err != nil {
			return fail(3, "boundary_invalid", "saved parent boundary for %s is not an ancestor; repair it explicitly", layer.Branch)
		}
		if err := a.repo.UpdateRef(snapshot+"/"+layer.Branch, head, ""); err != nil {
			return err
		}
		progress.Layers = append(progress.Layers, localstate.RestackLayer{Branch: layer.Branch, OldBase: layer.ParentSHA, NewBase: newBase, OriginalHead: head, State: "pending"})
		newBase = ""
	}
	if len(progress.Layers) == 0 {
		return fail(3, "precondition", "stack has no open layers")
	}
	progress.Phase = "running"
	if err := a.store.SaveRestack(progress); err != nil {
		return err
	}
	return a.runRestack(ctx, state, progress)
}

func (a *application) runRestack(ctx context.Context, state *localstate.State, progress *localstate.Restack) error {
	for progress.Current < len(progress.Layers) {
		layer := &progress.Layers[progress.Current]
		if progress.Current > 0 {
			layer.NewBase = progress.Layers[progress.Current-1].NewHead
		}
		layer.State, progress.Phase = "running", "running"
		if err := a.store.SaveRestack(progress); err != nil {
			return err
		}
		a.progress("restacking %s onto %s", layer.Branch, short(layer.NewBase))
		operation := "rebase"
		var err error
		if progress.Mode == api.StackModeMerge {
			operation, err = "merge", a.mergeLayer(ctx, state, progress)
		} else {
			err = a.repo.RebaseContext(ctx, layer.OldBase, layer.NewBase, layer.Branch, progress.Sign)
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError(operation, err)
			}
			if !a.repo.RebaseActive() && !a.repo.MergeActive() && len(a.repo.ConflictedFiles()) == 0 {
				progress.Phase = "failed"
				_ = a.store.SaveRestack(progress)
				return fmt.Errorf("restack %s: %w", layer.Branch, err)
			}
			layer.State, progress.Phase = "conflicted", "conflicted"
			if saveErr := a.store.SaveRestack(progress); saveErr != nil {
				return saveErr
			}
			return fail(5, "restack_conflict", "%s conflicts: %s", layer.Branch, strings.Join(a.repo.ConflictedFiles(), ", "))
		}
		newHead, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		layer.NewHead, layer.State = newHead, "done"
		progress.Current++
		if err := a.store.SaveRestack(progress); err != nil {
			return err
		}
	}
	return a.finishRestack(state, progress)
}

// mergeLayer merges the layer's new parent into it unless the layer already contains it.
func (a *application) mergeLayer(ctx context.Context, state *localstate.State, progress *localstate.Restack) error {
	layer := &progress.Layers[progress.Current]
	parentBranch := progress.Trunk
	if progress.Current > 0 {
		parentBranch = progress.Layers[progress.Current-1].Branch
	}
	message := fmt.Sprintf("Merge branch '%s' into %s", parentBranch, layer.Branch)
	head, err := a.repo.Head(layer.Branch)
	if err != nil || a.repo.IsAncestor(layer.NewBase, head) == nil {
		return err
	}
	if err := a.repo.Switch(layer.Branch); err != nil {
		return err
	}
	for _, landed := range state.Layers {
		if progress.Current != 0 || !a.squashedInto(landed, head) { // only the lowest open layer sits on the trunk where squashes land
			continue
		}
		if err := a.repo.MergeContext(ctx, landed.LandedSHA, message, progress.Sign, true); err != nil {
			return err
		}
		if head, err = a.repo.Head(layer.Branch); err != nil {
			return err
		}
		layer.NewHead = head // lets --abort restore this merge too
		if err := a.store.SaveRestack(progress); err != nil {
			return err
		}
		if a.repo.IsAncestor(layer.NewBase, head) == nil {
			return nil
		}
	}
	return a.repo.MergeContext(ctx, layer.NewBase, message, progress.Sign, false)
}

// squashedInto reports whether landed is a squash carrying exactly the content of a layer head that head already contains.
func (a *application) squashedInto(landed localstate.Layer, head string) bool {
	if landed.LandedSHA == "" || landed.HeadSHA == "" || a.repo.IsAncestor(landed.LandedSHA, head) == nil || a.repo.IsAncestor(landed.HeadSHA, head) != nil {
		return false
	}
	landedTree, err := a.repo.Run(nil, "rev-parse", "--verify", "--end-of-options", landed.LandedSHA+"^{tree}")
	if err != nil {
		return false
	}
	headTree, err := a.repo.Run(nil, "rev-parse", "--verify", "--end-of-options", landed.HeadSHA+"^{tree}")
	return err == nil && headTree == landedTree
}

func (a *application) continueRestack(ctx context.Context) error {
	progress, err := a.store.LoadRestack()
	if err != nil || (progress.Phase != "conflicted" && progress.Phase != "running" && progress.Phase != "failed") || progress.Current >= len(progress.Layers) {
		return fail(3, "precondition", "no conflicted restack to continue")
	}
	layer := &progress.Layers[progress.Current]
	if a.repo.RebaseActive() {
		if err := a.repo.RebaseContinueContext(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("rebase --continue", err)
			}
			_ = a.store.SaveRestack(progress)
			return fail(5, "restack_conflict", "%s conflicts: %s", layer.Branch, strings.Join(a.repo.ConflictedFiles(), ", "))
		}
	}
	if a.repo.MergeActive() {
		if err := a.repo.MergeCommitContext(ctx, progress.Sign); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("commit", err)
			}
			return fail(5, "restack_conflict", "%s conflicts: %s", layer.Branch, strings.Join(a.repo.ConflictedFiles(), ", "))
		}
	}
	newHead, err := a.repo.Head(layer.Branch)
	if err != nil {
		return err
	}
	done := newHead != layer.OriginalHead
	if progress.Mode == api.StackModeMerge {
		done = a.repo.IsAncestor(layer.NewBase, newHead) == nil
	}
	if !done {
		if progress.Phase == "running" || progress.Phase == "failed" {
			state, stateErr := a.state()
			if stateErr != nil {
				return stateErr
			}
			return a.runRestack(ctx, state, progress)
		}
		return a.abortRestack(ctx)
	}
	layer.NewHead, layer.State = newHead, "done"
	progress.Current++
	if err := a.store.SaveRestack(progress); err != nil {
		return err
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	return a.runRestack(ctx, state, progress)
}

func (a *application) abortRestack(ctx context.Context) error {
	progress, err := a.store.LoadRestack()
	if err != nil {
		return fail(3, "precondition", "no restack to abort")
	}
	if a.repo.RebaseActive() {
		if err := a.repo.RebaseAbortContext(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("rebase --abort", err)
			}
			return err
		}
	}
	if a.repo.MergeActive() {
		if err := a.repo.MergeAbortContext(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return mapGitContextError("merge --abort", err)
			}
			return err
		}
	}
	if _, err := a.repo.Run(nil, "switch", "--detach", progress.Layers[0].OriginalHead); err != nil {
		return err
	}
	for _, layer := range progress.Layers {
		worktrees, err := a.repo.WorktreesForBranch(layer.Branch)
		if err != nil {
			return err
		}
		if len(worktrees) != 0 {
			return fail(3, "worktree_in_use", "%s is checked out in another worktree: %s", layer.Branch, strings.Join(worktrees, ", "))
		}
		current, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		if layer.NewHead != "" && current == layer.NewHead {
			if err := a.repo.UpdateRef("refs/heads/"+layer.Branch, layer.OriginalHead, layer.NewHead); err != nil {
				return fail(3, "concurrent_edit", "refuse to overwrite concurrent change to %s: %v", layer.Branch, err)
			}
		} else if current != layer.OriginalHead {
			return fail(3, "concurrent_edit", "refuse to overwrite concurrent change to %s", layer.Branch)
		}
	}
	if err := a.repo.Switch(progress.OriginalBranch); err != nil {
		return err
	}
	if err := a.store.RemoveRestack(); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintln(os.Stdout, "restack aborted; backup refs retained under "+progress.Snapshot)
	}
	a.success(progress)
	return nil
}

func (a *application) finishRestack(state *localstate.State, progress *localstate.Restack) error {
	for _, result := range progress.Layers {
		for i := range state.Layers {
			if state.Layers[i].Branch == result.Branch {
				state.Layers[i].HeadSHA, state.Layers[i].ParentSHA = result.NewHead, result.NewBase
			}
		}
	}
	if err := a.store.Save(state); err != nil {
		return err
	}
	if err := a.repo.Switch(progress.OriginalBranch); err != nil {
		return err
	}
	progress.Phase = "done"
	if err := a.store.RemoveRestack(); err != nil {
		return err
	}
	if !a.jsonOutput {
		for _, layer := range progress.Layers {
			fmt.Fprintf(os.Stdout, "%s %s -> %s\n", layer.Branch, short(layer.OriginalHead), short(layer.NewHead))
		}
		fmt.Fprintln(os.Stdout, "nothing was pushed; run gitea-stack push")
	}
	a.success(progress)
	return nil
}

func (a *application) serverRebase(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rebase", flag.ContinueOnError)
	serverFlag := flags.Bool("server", false, "run on server")
	through := flags.Int("through", 0, "first affected position")
	revision := flags.Int64("revision", 0, "expected server revision")
	if err := flags.Parse(args); err != nil || !*serverFlag {
		return fail(2, "usage", "rebase requires --server")
	}
	state, _, err := a.optionalState()
	if err != nil {
		return err
	}
	number, err := a.serverStackNumber(state)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	mode := api.StackMode("")
	if state.Stack == number {
		mode = cmp.Or(state.Mode, api.StackModeRebase)
	}
	if expected == 0 || mode == "" {
		server, err := client.GetStack(ctx, number)
		if err != nil {
			return mapAPIError(err)
		}
		expected, mode = cmp.Or(expected, server.Revision), server.Mode
	}
	var op *api.PullRequestStackOperation
	if mode == api.StackModeMerge {
		if *through != 0 {
			return fail(2, "usage", "merge-mode server updates cover every open layer; omit --through")
		}
		op, err = client.StartUpdate(ctx, number, expected)
	} else {
		op, err = client.StartRebase(ctx, number, expected, *through)
	}
	if err != nil {
		return mapAPIError(err)
	}
	return a.printOperation(op)
}

func (a *application) land(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("land", flag.ContinueOnError)
	throughValue := flags.String("through", "", "required final layer")
	mergeStyle := flags.String("merge-style", "squash", "merge, squash, rebase (rebase mode) or fast-forward-only (merge mode)")
	wait := flags.Bool("wait", false, "wait for a terminal or blocked state")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait time")
	revision := flags.Int64("revision", 0, "expected server revision")
	if err := flags.Parse(args); err != nil || *throughValue == "" {
		return fail(2, "usage", "land requires --through")
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	number, err := a.boundStackNumber(state, false)
	if err != nil {
		return err
	}
	through, err := throughIndex(state, *throughValue)
	if err != nil {
		return err
	}
	if state.Mode == api.StackModeMerge && !slices.Contains([]string{"merge", "squash", "fast-forward-only"}, *mergeStyle) {
		return fail(2, "usage", "merge-mode stacks land with merge, squash or fast-forward-only")
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	if expected == 0 {
		server, err := client.GetStack(ctx, number)
		if err != nil {
			return mapAPIError(err)
		}
		if server.ActiveOperation != 0 {
			return fail(6, "operation_active", "stack has active operation %d", server.ActiveOperation)
		}
		expected = server.Revision
	}
	op, err := client.StartLand(ctx, number, expected, through, *mergeStyle)
	if err != nil {
		return mapAPIError(err)
	}
	if *wait {
		waitCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		op, err = client.WaitOperation(waitCtx, number, op.Number, 3*time.Second)
		if err != nil {
			return err
		}
		if op.State == "blocked" || op.State == "failed" {
			return fail(6, "operation_failed", "%s after %d layers: %s", op.State, op.Completed, op.Error)
		}
	}
	return a.printOperation(op)
}

func (a *application) operation(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fail(2, "usage", "op requires list, status, wait, cancel, or retry")
	}
	state, _, err := a.optionalState()
	if err != nil {
		return err
	}
	stackNumber, err := a.serverStackNumber(state)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	if args[0] == "list" {
		ops, err := client.ListOperations(ctx, stackNumber, 1, 100)
		if err != nil {
			return mapAPIError(err)
		}
		if !a.jsonOutput {
			for _, op := range ops {
				fmt.Fprintf(os.Stdout, "%d %s %s %d/%d\n", op.Number, op.Kind, op.State, op.Completed, op.ThroughPosition)
			}
		}
		a.success(ops)
		return nil
	}
	if len(args) != 2 {
		return fail(2, "usage", "op %s requires an operation number", args[0])
	}
	number, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fail(2, "usage", "invalid operation number")
	}
	var op *api.PullRequestStackOperation
	switch args[0] {
	case "status":
		op, err = client.GetOperation(ctx, stackNumber, number)
	case "wait":
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		op, err = client.WaitOperation(waitCtx, stackNumber, number, 3*time.Second)
	case "cancel":
		err = client.CancelOperation(ctx, stackNumber, number)
		if err == nil {
			op, err = client.GetOperation(ctx, stackNumber, number)
		}
	case "retry":
		op, err = client.RetryOperation(ctx, stackNumber, number)
	default:
		return fail(2, "usage", "unknown op command %q", args[0])
	}
	if err != nil {
		return mapAPIError(err)
	}
	return a.printOperation(op)
}

func (a *application) printOperation(op *api.PullRequestStackOperation) error {
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "operation %d %s %s %d/%d\n", op.Number, op.Kind, op.State, op.Completed, op.ThroughPosition)
		if op.Error != "" {
			fmt.Fprintln(os.Stderr, op.Error)
		}
	}
	a.success(op)
	return nil
}

func (a *application) unstack(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("unstack", flag.ContinueOnError)
	revision := flags.Int64("revision", 0, "expected server revision")
	if err := flags.Parse(args); err != nil {
		return fail(2, "usage", "%v", err)
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	number, err := a.boundStackNumber(state, false)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	if expected == 0 {
		server, err := client.GetStack(ctx, number)
		if err != nil {
			return mapAPIError(err)
		}
		expected = server.Revision
	}
	if err := client.Unstack(ctx, number, expected); err != nil {
		return mapAPIError(err)
	}
	if err := a.store.Remove(); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "unstacked S%d; branches and pull requests were kept\n", number)
	}
	a.success(map[string]int64{"stack": number})
	return nil
}

func (a *application) capabilities(ctx context.Context) error {
	client, err := a.client(nil)
	if err != nil {
		return err
	}
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "enabled: %t; operations: %s; merge styles: %s\n", capabilities.Enabled, strings.Join(capabilities.Operations, ","), strings.Join(capabilities.MergeStyles, ","))
	}
	a.success(capabilities)
	return nil
}

func (a *application) snapshots(args []string) error {
	if len(args) == 0 || args[0] == "prune" {
		return fail(2, "usage", "snapshots supports list or restore <timestamp> <branch>")
	}
	switch args[0] {
	case "list":
		refs, err := a.repo.Run(nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/gitea-stack/backup/")
		if err != nil {
			return err
		}
		if !a.jsonOutput {
			fmt.Fprintln(os.Stdout, refs)
		}
		a.success(map[string]string{"refs": refs})
		return nil
	case "restore":
		if len(args) != 3 {
			return fail(2, "usage", "snapshots restore requires a timestamp and branch")
		}
		if err := a.repo.RequireClean(); err != nil {
			return fail(3, "precondition", "%v", err)
		}
		if err := a.repo.ValidateBranch(args[2]); err != nil {
			return fail(2, "usage", "invalid branch: %v", err)
		}
		backup := "refs/gitea-stack/backup/" + args[1] + "/" + args[2]
		sha, err := a.repo.Head(backup)
		if err != nil {
			return fail(8, "not_found", "%s", backup)
		}
		current, err := a.repo.Head(args[2])
		if err != nil {
			return err
		}
		if branch, _ := a.repo.CurrentBranch(); branch == args[2] {
			return fail(3, "precondition", "check out another branch before restoring %s", args[2])
		}
		if err := a.repo.UpdateRef("refs/heads/"+args[2], sha, current); err != nil {
			return fail(3, "concurrent_edit", "%v", err)
		}
		if !a.jsonOutput {
			fmt.Fprintf(os.Stdout, "restored %s to %s\n", args[2], short(sha))
		}
		a.success(map[string]string{"branch": args[2], "sha": sha})
		return nil
	default:
		return fail(2, "usage", "unknown snapshots command %q", args[0])
	}
}
