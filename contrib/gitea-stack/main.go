// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gitea.dev/contrib/gitea-stack/internal/gitx"
	"gitea.dev/contrib/gitea-stack/internal/localstate"
	"gitea.dev/modules/json"
	"gitea.dev/modules/stackclient"
	api "gitea.dev/modules/structs"
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
	stack := global.String("stack", "", "stack number (S12 or 12)")
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
	repo := gitx.Repo{Dir: "."}
	store, err := localstate.Open(repo)
	if err != nil {
		return reportError(err, *jsonOutput)
	}
	app := &application{repo: repo, store: store, jsonOutput: *jsonOutput, quiet: *quiet, remoteFlag: *remote, stackFlag: *stack, commandName: args[0]}
	unlock, err := store.Lock()
	if err != nil {
		return reportError(fail(3, "locked", "%v", err), *jsonOutput)
	}
	defer unlock()
	if store.RestackExists() && !(args[0] == "restack" || args[0] == "snapshots") {
		return reportError(fail(3, "restack_in_progress", "restack in progress; run restack --continue or --abort"), *jsonOutput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
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
		return a.push(ctx, args, true)
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
		return a.restack(args)
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
	token := os.Getenv("GITEA_TOKEN")
	if token == "" {
		token = os.Getenv("GITEA_STACK_TOKEN")
	}
	if token == "" {
		return nil, fail(7, "missing_token", "set GITEA_TOKEN")
	}
	remote, err := a.selectedRemote(state)
	if err != nil {
		return nil, err
	}
	remoteURL, err := a.repo.RemoteURL(remote)
	if err != nil {
		return nil, err
	}
	client, err := stackclient.FromRemote(remoteURL, token)
	if err != nil {
		return nil, err
	}
	return client, nil
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

func (a *application) init(args []string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	trunk := flags.String("trunk", "", "trunk branch")
	remoteFlag := flags.String("remote", "", "git remote")
	if err := flags.Parse(args); err != nil || *trunk == "" {
		return fail(2, "usage", "init requires --trunk and explicit ordered branches")
	}
	if err := a.repo.RequireClean(); err != nil {
		return fail(3, "precondition", "%v", err)
	}
	if err := a.repo.ValidateBranch(*trunk); err != nil {
		return fail(2, "usage", "invalid trunk: %v", err)
	}
	remote := *remoteFlag
	if remote == "" {
		var err error
		remote, err = a.selectedRemote(nil)
		if err != nil {
			return err
		}
	}
	parentSHA, err := a.repo.Head(*trunk)
	if err != nil {
		return fail(8, "not_found", "trunk %s: %v", *trunk, err)
	}
	state := &localstate.State{Remote: remote, Trunk: *trunk, LastSyncedTrunkSHA: parentSHA, Layers: []localstate.Layer{}}
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

func (a *application) stackNumber(state *localstate.State) (int64, error) {
	if a.stackFlag != "" {
		return parseStack(a.stackFlag)
	}
	if state.Stack == 0 {
		return 0, fail(8, "not_found", "local stack has not been submitted")
	}
	return state.Stack, nil
}

func (a *application) status(ctx context.Context) error {
	state, err := a.state()
	if err != nil {
		return err
	}
	var server *api.PullRequestStack
	if state.Stack != 0 && (os.Getenv("GITEA_TOKEN") != "" || os.Getenv("GITEA_STACK_TOKEN") != "") {
		if client, clientErr := a.client(state); clientErr == nil {
			server, _ = client.GetStack(ctx, state.Stack)
		}
	}
	if !a.jsonOutput {
		op := int64(0)
		if server != nil {
			op = server.ActiveOperation
		}
		fmt.Fprintf(os.Stdout, "S%d on %s  rev %d  op %d\n", state.Stack, state.Trunk, state.LastRevision, op)
		for i, layer := range state.Layers {
			status := "open"
			if layer.LandedSHA != "" {
				status = "merged " + short(layer.LandedSHA)
			}
			fmt.Fprintf(os.Stdout, " %d  %s  #%d  %s\n", i+1, layer.Branch, layer.PullRequest, status)
		}
	}
	a.success(map[string]any{"local": state, "server": server})
	return nil
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

func (a *application) pushLayers(state *localstate.State, through int) error {
	for i := range through {
		layer := &state.Layers[i]
		if layer.LandedSHA != "" {
			continue
		}
		head, err := a.repo.Head(layer.Branch)
		if err != nil {
			return err
		}
		remoteHead, err := a.repo.RemoteHead(state.Remote, layer.Branch)
		if err != nil {
			return fail(6, "remote_failed", "%v", err)
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
		if err := a.repo.PushLease(state.Remote, layer.Branch, layer.RemoteSHA); err != nil {
			return fail(6, "lease_rejected", "%s: %v", layer.Branch, err)
		}
		layer.HeadSHA, layer.RemoteSHA = head, head
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) push(ctx context.Context, args []string, synchronize bool) error {
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
	through, err := throughIndex(state, *throughValue)
	if err != nil {
		return err
	}
	var client *stackclient.Client
	var server *api.PullRequestStack
	if synchronize && state.Stack != 0 {
		client, err = a.client(state)
		if err != nil {
			return err
		}
		server, err = client.GetStack(ctx, state.Stack)
		if err != nil {
			return mapAPIError(err)
		}
		if server.ActiveOperation != 0 {
			return fail(6, "operation_active", "stack has active operation %d", server.ActiveOperation)
		}
		for i := through; i < len(state.Layers); i++ {
			if state.Layers[i].LandedSHA == "" {
				return fail(3, "precondition", "a submitted stack push must include every open layer so server boundaries stay complete")
			}
		}
	}
	if err := a.pushLayers(state, through); err != nil {
		return err
	}
	if client != nil {
		heads := make([]api.PullRequestStackHead, 0, len(state.Layers))
		for _, layer := range state.Layers {
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
			heads = append(heads, api.PullRequestStackHead{PullRequest: layer.PullRequest, HeadSHA: head, ParentSHA: layer.ParentSHA})
		}
		server, err = client.SynchronizeStack(ctx, state.Stack, server.Revision, heads)
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
	if err := a.pushLayers(state, through); err != nil {
		return err
	}
	newPulls := make([]int64, 0)
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
		newPulls = append(newPulls, pull.Index)
		if err := a.store.Save(state); err != nil {
			return err
		}
	}
	if state.Stack == 0 {
		pulls := make([]int64, 0, through)
		for i := range through {
			pulls = append(pulls, state.Layers[i].PullRequest)
		}
		server, err := client.CreateStack(ctx, state.Trunk, pulls)
		if err != nil {
			return mapAPIError(err)
		}
		state.Stack, state.LastRevision = server.Number, server.Revision
	} else if len(newPulls) != 0 {
		server, err := client.GetStack(ctx, state.Stack)
		if err != nil {
			return mapAPIError(err)
		}
		server, err = client.AppendStack(ctx, state.Stack, server.Revision, newPulls)
		if err != nil {
			return mapAPIError(err)
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

func (a *application) adopt(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("adopt", flag.ContinueOnError)
	pullValue := flags.String("prs", "", "ordered pull request numbers")
	trunk := flags.String("trunk", "", "trunk branch")
	if err := flags.Parse(args); err != nil || *pullValue == "" || *trunk == "" {
		return fail(2, "usage", "adopt requires --prs and --trunk")
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
	for _, number := range pulls {
		pull, err := client.GetPull(ctx, number)
		if err != nil {
			return mapAPIError(err)
		}
		if pull.Base.Ref != parent || pull.Head == nil || pull.Head.Ref == "" {
			return fail(3, "invalid_chain", "#%d does not target %s", number, parent)
		}
		apiPulls = append(apiPulls, pull)
		branches = append(branches, pull.Head.Ref)
		parent = pull.Head.Ref
	}
	if err := a.repo.Fetch(remote, branches); err != nil {
		return fail(6, "remote_failed", "%v", err)
	}
	trunkSHA, err := a.repo.Head("refs/remotes/" + remote + "/" + *trunk)
	if err != nil {
		return err
	}
	state := &localstate.State{Remote: remote, Trunk: *trunk, LastSyncedTrunkSHA: trunkSHA}
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
		if parentSHA == "" || a.repo.IsAncestor(parentSHA, localHead) != nil {
			return fail(3, "boundary_invalid", "#%d has no verifiable saved parent boundary", pull.Index)
		}
		state.Layers = append(state.Layers, localstate.Layer{Branch: pull.Head.Ref, PullRequest: pull.Index, HeadSHA: localHead, RemoteSHA: remoteHead, ParentSHA: parentSHA})
	}
	server, err := client.CreateStack(ctx, *trunk, pulls)
	if err != nil {
		return mapAPIError(err)
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

func (a *application) sync(ctx context.Context) error {
	state, err := a.state()
	if err != nil {
		return err
	}
	number, err := a.stackNumber(state)
	if err != nil {
		return err
	}
	branches := []string{state.Trunk}
	for _, layer := range state.Layers {
		branches = append(branches, layer.Branch)
	}
	if err := a.repo.Fetch(state.Remote, branches); err != nil {
		return fail(6, "remote_failed", "%v", err)
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	server, err := client.GetStack(ctx, number)
	if err != nil {
		return mapAPIError(err)
	}
	trunkSHA, err := a.repo.Head("refs/remotes/" + state.Remote + "/" + state.Trunk)
	if err != nil {
		return err
	}
	needsRestack := make([]string, 0)
	needsReconciliation := make([]string, 0)
	entriesByPull := make(map[int64]*api.PullRequestStackEntry, len(server.Entries))
	for _, entry := range server.Entries {
		if entry.PullRequest != nil {
			entriesByPull[entry.PullRequest.Index] = entry
		}
	}
	openParent := trunkSHA
	for i := range state.Layers {
		layer := &state.Layers[i]
		localHead, _ := a.repo.Head(layer.Branch)
		remoteSHA, err := a.repo.Head("refs/remotes/" + state.Remote + "/" + layer.Branch)
		entry := entriesByPull[layer.PullRequest]
		if err == nil && remoteLeaseCanAdvance(a.repo, localHead, layer.RemoteSHA, remoteSHA, entry) {
			layer.RemoteSHA = remoteSHA
		}
		if entry != nil {
			layer.LandedSHA = entry.LandedSHA
		}
		if layer.LandedSHA != "" {
			openParent = trunkSHA
			continue
		}
		if remoteSHA != "" && remoteSHA != layer.RemoteSHA {
			needsReconciliation = append(needsReconciliation, layer.Branch)
		}
		if layer.ParentSHA != openParent {
			needsRestack = append(needsRestack, layer.Branch)
		}
		if remoteSHA != "" {
			openParent = remoteSHA
		}
	}
	state.LastRevision, state.LastSyncedTrunkSHA = server.Revision, trunkSHA
	if err := a.store.Save(state); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "S%d rev %d; restack: %s\n", number, server.Revision, strings.Join(needsRestack, ", "))
		if len(needsReconciliation) != 0 {
			fmt.Fprintf(os.Stdout, "Reconcile remote changes before pushing: %s; previous leases retained\n", strings.Join(needsReconciliation, ", "))
		}
	}
	a.success(map[string]any{"stack": state, "needs_restack": needsRestack, "needs_reconciliation": needsReconciliation})
	return nil
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

func (a *application) restack(args []string) error {
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
		return a.abortRestack()
	}
	if *continueFlag {
		return a.continueRestack()
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
	progress := &localstate.Restack{Phase: "planning", Stack: state.Stack, Trunk: state.Trunk, Sign: signValue, Snapshot: snapshot, OriginalBranch: original}
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
	return a.runRestack(state, progress)
}

func (a *application) runRestack(state *localstate.State, progress *localstate.Restack) error {
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
		if err := a.repo.Rebase(layer.OldBase, layer.NewBase, layer.Branch, progress.Sign); err != nil {
			if !a.repo.RebaseActive() && len(a.repo.ConflictedFiles()) == 0 {
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

func (a *application) continueRestack() error {
	progress, err := a.store.LoadRestack()
	if err != nil || (progress.Phase != "conflicted" && progress.Phase != "running" && progress.Phase != "failed") || progress.Current >= len(progress.Layers) {
		return fail(3, "precondition", "no conflicted restack to continue")
	}
	layer := &progress.Layers[progress.Current]
	if a.repo.RebaseActive() {
		if err := a.repo.RebaseContinue(); err != nil {
			_ = a.store.SaveRestack(progress)
			return fail(5, "restack_conflict", "%s conflicts: %s", layer.Branch, strings.Join(a.repo.ConflictedFiles(), ", "))
		}
	}
	newHead, err := a.repo.Head(layer.Branch)
	if err != nil {
		return err
	}
	if newHead == layer.OriginalHead {
		if progress.Phase == "running" || progress.Phase == "failed" {
			state, stateErr := a.state()
			if stateErr != nil {
				return stateErr
			}
			return a.runRestack(state, progress)
		}
		return a.abortRestack()
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
	return a.runRestack(state, progress)
}

func (a *application) abortRestack() error {
	progress, err := a.store.LoadRestack()
	if err != nil {
		return fail(3, "precondition", "no restack to abort")
	}
	if a.repo.RebaseActive() {
		if err := a.repo.RebaseAbort(); err != nil {
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
	state, err := a.state()
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	if expected == 0 {
		server, err := client.GetStack(ctx, state.Stack)
		if err != nil {
			return mapAPIError(err)
		}
		expected = server.Revision
	}
	op, err := client.StartRebase(ctx, state.Stack, expected, *through)
	if err != nil {
		return mapAPIError(err)
	}
	return a.printOperation(op)
}

func (a *application) land(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("land", flag.ContinueOnError)
	throughValue := flags.String("through", "", "required final layer")
	mergeStyle := flags.String("merge-style", "squash", "merge, squash, or rebase")
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
	through, err := throughIndex(state, *throughValue)
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	if expected == 0 {
		server, err := client.GetStack(ctx, state.Stack)
		if err != nil {
			return mapAPIError(err)
		}
		if server.ActiveOperation != 0 {
			return fail(6, "operation_active", "stack has active operation %d", server.ActiveOperation)
		}
		expected = server.Revision
	}
	op, err := client.StartLand(ctx, state.Stack, expected, through, *mergeStyle)
	if err != nil {
		return mapAPIError(err)
	}
	if *wait {
		waitCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		op, err = client.WaitOperation(waitCtx, state.Stack, op.Number, 3*time.Second)
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
	state, err := a.state()
	if err != nil {
		return err
	}
	client, err := a.client(state)
	if err != nil {
		return err
	}
	if args[0] == "list" {
		ops, err := client.ListOperations(ctx, state.Stack, 1, 100)
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
		op, err = client.GetOperation(ctx, state.Stack, number)
	case "wait":
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		op, err = client.WaitOperation(waitCtx, state.Stack, number, 3*time.Second)
	case "cancel":
		err = client.CancelOperation(ctx, state.Stack, number)
		if err == nil {
			op, err = client.GetOperation(ctx, state.Stack, number)
		}
	case "retry":
		op, err = client.RetryOperation(ctx, state.Stack, number)
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
	client, err := a.client(state)
	if err != nil {
		return err
	}
	expected := *revision
	if expected == 0 {
		server, err := client.GetStack(ctx, state.Stack)
		if err != nil {
			return mapAPIError(err)
		}
		expected = server.Revision
	}
	if err := client.Unstack(ctx, state.Stack, expected); err != nil {
		return mapAPIError(err)
	}
	if err := a.store.Remove(); err != nil {
		return err
	}
	if !a.jsonOutput {
		fmt.Fprintf(os.Stdout, "unstacked S%d; branches and pull requests were kept\n", state.Stack)
	}
	a.success(map[string]int64{"stack": state.Stack})
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
