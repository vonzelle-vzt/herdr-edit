// =============================================================================
// File: internal/dap/launchselect.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// launchselect.go turns a parsed launch.json entry into something this editor
// can actually hand an adapter: variables expanded, an adapter chosen by the
// configuration's `type` rather than by the file on screen, and the merge order
// that makes the user's file authoritative.
//
// It is the half launchjson.go was missing. That file could read a launch.json
// perfectly and had NO non-test caller, which is the fork-wide pattern CLAUDE.md
// records: a green test suite proves the engine works, not that anyone can reach
// it. Everything here exists to be called from internal/app/launchpicker.go.
//
// # Three decisions, and each has a silent failure behind it
//
//   - SELECTION IS BY `type`, NOT BY LANGUAGE. AdapterFor scans Adapter.Languages
//     first-match-wins, which is the right rule for F5 on a source file and the
//     wrong one for a configuration that names what it wants. It is also the only
//     way a browser adapter can exist at all: js-debug's node row already claims
//     `javascript`, so a chrome row claiming it would silently steal every Node
//     session. See Adapter.ConfigTypes.
//
//   - THE CONFIG WINS, THE TYPE DOES NOT. Merging starts from the adapter's own
//     Launch defaults and the user's keys overwrite them — but `type` is then
//     FORCED back to the adapter's canonical id. The standalone js-debug server
//     dispatches on `pwa-node` / `pwa-chrome`; the friendly `node` and `chrome`
//     aliases are registered by the VS Code extension's package.json, which we do
//     not have and never load. Passing the user's `"type": "chrome"` through
//     reaches an adapter that does not know the word.
//
//   - AN UNRESOLVABLE VARIABLE REFUSES. ${command:...} and ${input:...} are
//     answered by a VS Code prompt this editor has no equivalent of. Expanding
//     them to nothing would launch SOMETHING — a program at a truncated path, a
//     browser at a blank url — and the user would be debugging a different thing
//     than the one they configured, with nothing on screen to say so.
package dap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LaunchFile is one project's launch.json, whole.
//
// 🔴 Compounds are READ, not ignored, and that is why this type exists rather
// than a bare slice. A user with a compound configuration who saw only the
// `configurations` array would find their entry simply MISSING from the picker,
// which reads as a broken picker rather than as an unsupported feature. The
// editor debugs one target at a time (see CLAUDE.md's js-debug section), so a
// compound is refused BY NAME — offered, and then explained — which is the only
// version that tells the truth.
type LaunchFile struct {
	Configurations []LaunchConfig
	Compounds      []LaunchCompound
}

// LaunchCompound is one `compounds` entry: several configurations the user
// wants started together.
type LaunchCompound struct {
	// Name is what the user sees, and what a refusal must name.
	Name string
	// Configurations are the member configuration names, in file order.
	Configurations []string
}

// LaunchVarContext is the state a ${...} variable resolves against: the project
// and the file the user is looking at.
type LaunchVarContext struct {
	// WorkspaceFolder is the project root, absolute. ${workspaceFolder}.
	WorkspaceFolder string
	// File is the active tab's absolute path, or "" when there is no file.
	// Every ${file...} variable resolves against it, and a configuration that
	// needs one while it is empty is REFUSED rather than launched with a hole
	// in it.
	File string
}

// LaunchSpec is a fully resolved launch: which adapter, which verb, and the
// exact map that goes on the wire.
//
// 🔴 It is the SINGLE argument to the start path, and that is the point.
// runDebugSession used to take (adapter, program), so a launch.json path would
// have needed either a second start function or a growing parameter list — and
// a second start function means the coordinator sequence, adoptChildSession and
// the verbatim child configuration all exist twice, with the copy being where
// breakpoint binding quietly stops.
type LaunchSpec struct {
	// Name is what to call this session on screen: the configuration's own name,
	// or the adapter's when F5 was pressed on a file.
	Name string
	// Adapter is the row that will be started.
	Adapter Adapter
	// Request is "launch" or "attach" — the actual DAP request to send.
	Request string
	// Target names what is being run, for the status bar and the published
	// panel payload: the program, the url, or failing both the name.
	Target string
	// Args is the complete configuration object for the wire.
	Args map[string]interface{}
}

// LaunchJSONFile is the absolute path of a project's launch.json. Exported so a
// caller can stat it — the editor drops a remembered configuration choice when
// this file changes, and a stat is much cheaper than a re-read.
func LaunchJSONFile(root string) string {
	return filepath.Join(root, filepath.FromSlash(LaunchJSONPath))
}

// LoadLaunchFile reads and parses a project's whole launch.json.
//
// A missing file is not an error — most projects have none. A malformed one IS:
// the user wrote something they expect to be used, and silently ignoring it
// would leave them wondering why their configuration does nothing.
func LoadLaunchFile(root string) (LaunchFile, error) {
	data, err := os.ReadFile(LaunchJSONFile(root))
	if err != nil {
		if os.IsNotExist(err) {
			return LaunchFile{}, nil
		}
		return LaunchFile{}, err
	}
	return ParseLaunchFile(data)
}

// ParseLaunchFile parses launch.json content that has already been read.
// Separate from LoadLaunchFile so the parsing is testable without a filesystem.
func ParseLaunchFile(data []byte) (LaunchFile, error) {
	var doc struct {
		Version        string                   `json:"version"`
		Configurations []map[string]interface{} `json:"configurations"`
		Compounds      []map[string]interface{} `json:"compounds"`
	}
	if err := json.Unmarshal(StripJSONC(data), &doc); err != nil {
		return LaunchFile{}, fmt.Errorf("launch.json: %w", err)
	}

	out := LaunchFile{Configurations: make([]LaunchConfig, 0, len(doc.Configurations))}
	for _, cfg := range doc.Configurations {
		out.Configurations = append(out.Configurations, LaunchConfig{
			Name:    stringField(cfg, "name"),
			Type:    stringField(cfg, "type"),
			Request: stringField(cfg, "request"),
			Args:    cfg,
		})
	}
	for _, c := range doc.Compounds {
		out.Compounds = append(out.Compounds, LaunchCompound{
			Name:           stringField(c, "name"),
			Configurations: compoundMembers(c["configurations"]),
		})
	}
	return out, nil
}

// compoundMembers reads a compound's member list, which VS Code allows to hold
// either a plain configuration name or a {folder, name} object for a
// multi-root workspace. Anything else is skipped rather than rendered as a
// Go map literal into a message the user has to read.
func compoundMembers(v interface{}) []string {
	list, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]interface{}:
			if name := stringField(t, "name"); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Variable expansion
// -----------------------------------------------------------------------------

// launchVarNeedsUI lists the ${...} prefixes VS Code answers with a prompt. They
// are refused rather than expanded — see this file's header.
var launchVarNeedsUI = []string{"${command:", "${input:"}

// ExpandLaunchVars replaces the VS Code launch variables this editor can resolve
// in one string, leaving anything it does not recognise untouched.
//
// 🔴 Unrecognised tokens SURVIVE on purpose. delve and js-debug both do their
// own substitution pass, so eating a variable they understand would be worse
// than passing it along — and the two we must never pass along
// (${command:...}, ${input:...}) are caught by the refusal check in
// ResolveLaunchConfig, which can only see them if they are still there.
func ExpandLaunchVars(s string, ctx LaunchVarContext) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if !strings.HasPrefix(s[i:], "${") {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			// An unterminated ${ is not a variable; copy the rest verbatim
			// rather than looping forever looking for a brace that never comes.
			b.WriteString(s[i:])
			break
		}
		name := s[i+2 : i+end]
		if val, ok := launchVarValue(name, ctx); ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[i : i+end+1])
		}
		i += end + 1
	}
	return b.String()
}

// launchVarValue resolves one variable name, reporting whether it is one we
// know at all. A known variable whose input is missing (${file} with no file
// open) resolves to "" and is caught by launchVarIsUnresolvable, which is what
// turns it into a refusal instead of an empty path.
func launchVarValue(name string, ctx LaunchVarContext) (string, bool) {
	if env, ok := strings.CutPrefix(name, "env:"); ok {
		return os.Getenv(env), true
	}
	switch name {
	case "workspaceFolder", "cwd", "workspaceRoot":
		return ctx.WorkspaceFolder, true
	case "workspaceFolderBasename":
		return filepath.Base(ctx.WorkspaceFolder), true
	case "file":
		return ctx.File, true
	case "fileDirname":
		return filepath.Dir(ctx.File), true
	case "fileBasename":
		return filepath.Base(ctx.File), true
	case "fileBasenameNoExtension":
		base := filepath.Base(ctx.File)
		return strings.TrimSuffix(base, filepath.Ext(base)), true
	case "fileExtname":
		return filepath.Ext(ctx.File), true
	case "relativeFile":
		return launchRelative(ctx.WorkspaceFolder, ctx.File), true
	case "relativeFileDirname":
		return launchRelative(ctx.WorkspaceFolder, filepath.Dir(ctx.File)), true
	case "pathSeparator", "/":
		return string(filepath.Separator), true
	case "userHome":
		if home, err := os.UserHomeDir(); err == nil {
			return home, true
		}
		return "", true
	}
	return "", false
}

// launchRelative renders p relative to base, falling back to p when the two
// share no prefix. A ../../.. chain out of the project would be worse than an
// absolute path, and both are better than an error for a cosmetic variable.
func launchRelative(base, p string) string {
	if base == "" || p == "" {
		return p
	}
	rel, err := filepath.Rel(base, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}

// launchFileVars are the variables that resolve against the ACTIVE FILE, listed
// so a configuration needing one with no file open can be refused by name.
var launchFileVars = []string{
	"${file}", "${fileDirname}", "${fileBasename}",
	"${fileBasenameNoExtension}", "${fileExtname}",
	"${relativeFile}", "${relativeFileDirname}",
}

// unresolvableIn reports which variables in s this editor cannot answer.
//
// Two kinds, and both would otherwise launch something plausible and wrong:
// the interactive ones VS Code prompts for, and the file-relative ones asked
// for while no file is open — which expand to "" and turn `program` into a
// path that is not there.
func unresolvableIn(s string, ctx LaunchVarContext) []string {
	var out []string
	for _, prefix := range launchVarNeedsUI {
		for i := 0; ; {
			at := strings.Index(s[i:], prefix)
			if at < 0 {
				break
			}
			at += i
			end := strings.IndexByte(s[at:], '}')
			if end < 0 {
				break
			}
			out = append(out, s[at:at+end+1])
			i = at + end + 1
		}
	}
	if ctx.File == "" {
		for _, v := range launchFileVars {
			if strings.Contains(s, v) {
				out = append(out, v+" (no file is open)")
			}
		}
	}
	return out
}

// expandValue walks a decoded JSON value, expanding variables in every string
// it contains and collecting anything unresolvable into bad.
//
// 🔴 It recurses rather than touching only the top-level string keys. `args`,
// `outFiles`, `env` and `sourceMapPathOverrides` are all arrays or objects full
// of ${workspaceFolder}, and a version that expanded only `program` would hand
// the adapter a correct program with an argument list still full of literal
// dollar-brace text — which most adapters accept without complaint.
func expandValue(v interface{}, ctx LaunchVarContext, bad *[]string) interface{} {
	switch t := v.(type) {
	case string:
		*bad = append(*bad, unresolvableIn(t, ctx)...)
		return ExpandLaunchVars(t, ctx)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = expandValue(item, ctx, bad)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, item := range t {
			out[k] = expandValue(item, ctx, bad)
		}
		return out
	}
	return v
}

// -----------------------------------------------------------------------------
// Resolution
// -----------------------------------------------------------------------------

// ResolveLaunchConfig turns one launch.json entry into a wire-ready LaunchSpec.
//
// The merge order is the contract, and it is deliberately not the obvious one:
//
//  1. start from the adapter's own Launch defaults — outputMode, console mode,
//     justMyCode, every measured key that makes the debuggee's output come back;
//  2. overwrite with the USER's keys, expanded. Their file is authoritative;
//  3. force `type` to the adapter's canonical id, because the standalone servers
//     do not know the aliases the VS Code extension registers;
//  4. send the workspace folder under the adapter's own key, when it has one;
//  5. inject `program` from the active tab ONLY when the configuration named
//     neither a program nor a url — a config that named one meant it.
func ResolveLaunchConfig(cfg LaunchConfig, ctx LaunchVarContext) (LaunchSpec, error) {
	adapter, ok := AdapterForConfigType(cfg.Type)
	if !ok {
		return LaunchSpec{}, fmt.Errorf("%q has type %q, which no adapter here handles (this editor knows: %s)",
			cfg.Name, cfg.Type, strings.Join(KnownConfigTypes(), ", "))
	}

	args := make(map[string]interface{}, len(adapter.Launch)+len(cfg.Args)+2)
	for k, v := range adapter.Launch {
		args[k] = v
	}

	var bad []string
	for k, v := range cfg.Args {
		args[k] = expandValue(v, ctx, &bad)
	}
	if len(bad) > 0 {
		return LaunchSpec{}, fmt.Errorf("%q uses %s, which this editor cannot answer — "+
			"VS Code prompts for those; replace them with literal values to run it here",
			cfg.Name, strings.Join(dedupe(bad), ", "))
	}

	// 🔴 Forced, not defaulted. See this file's header: `chrome` and `node` are
	// aliases the VS Code extension registers and the standalone server has
	// never heard of.
	args["type"] = adapter.AdapterID

	// 🔴 MEASURED in the installed bundle, and without it browser debugging is
	// broken while looking perfectly healthy. js-debug's chrome defaults carry
	// `webRoot: "${workspaceFolder}"`, and its resolver reads __workspaceFolder
	// off the configuration to expand it — when that key is absent it takes a
	// fallback branch that sets `webRoot = "/"` outright. Every source url then
	// maps to nothing on disk, so no breakpoint ever binds and the session
	// reports no error at all. Node never needed it, which is why this stayed
	// invisible through the whole js-debug stage.
	if adapter.WorkspaceFolderKey != "" && ctx.WorkspaceFolder != "" {
		args[adapter.WorkspaceFolderKey] = ctx.WorkspaceFolder
	}

	_, hasProgram := args["program"]
	_, hasURL := args["url"]
	if !hasProgram && !hasURL && ctx.File != "" {
		program := ctx.File
		if adapter.ProgramIsDir {
			program = filepath.Dir(ctx.File)
		}
		args["program"] = program
	}

	request := cfg.Request
	if request == "" {
		request = stringField(args, "request")
	}
	if request == "" {
		request = "launch"
	}
	args["request"] = request

	name := cfg.Name
	if name == "" {
		name = adapter.Name
	}
	return LaunchSpec{
		Name:    name,
		Adapter: adapter,
		Request: request,
		Target:  launchTarget(args, name),
		Args:    args,
	}, nil
}

// SpecForFile is the language-keyed path — F5 on a source file — expressed as a
// LaunchSpec, so it and the launch.json path go down the SAME start function.
//
// 🔴 It is deliberately the adapter's Launch defaults plus `program` and
// nothing else, byte for byte with what the start path built inline before this
// file existed. In particular it does NOT send a workspace folder: the live
// js-debug oracle proves the node path works exactly as it is, and quietly
// adding a key to the one path that is already covered by a live oracle would
// invalidate the measurement that says so.
func SpecForFile(adapter Adapter, program string) LaunchSpec {
	args := make(map[string]interface{}, len(adapter.Launch)+1)
	for k, v := range adapter.Launch {
		args[k] = v
	}
	args["program"] = program

	request := stringField(args, "request")
	if request == "" {
		request = "launch"
	}
	return LaunchSpec{
		Name:    adapter.Name,
		Adapter: adapter,
		Request: request,
		Target:  program,
		Args:    args,
	}
}

// launchTarget names what a spec runs, preferring the program, then the url,
// then the configuration's own name. It is what the status bar and the
// published panel payload show, so "" would read as a session running nothing.
func launchTarget(args map[string]interface{}, name string) string {
	if p := stringField(args, "program"); p != "" {
		return p
	}
	if u := stringField(args, "url"); u != "" {
		return u
	}
	return name
}

// dedupe removes repeats while keeping first-seen order, so a variable used in
// three keys is named once in the refusal.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
