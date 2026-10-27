// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package highlight

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// patchChromaLexers works around Chroma lexer bugs until they are fixed upstream
func patchChromaLexers() {
	// The Vue lexer is "dot_all", so the greedy `=".+` of "v-*" directives makes a multi-line value
	// swallow everything up to the last quote of the file. TODO: remove after https://github.com/alecthomas/chroma/pull/1419
	vue, ok := lexers.GlobalLexerRegistry.Get("vue").(*chroma.RegexLexer)
	if !ok {
		return
	}
	lexers.Register(chroma.MustNewLexer(vue.Config(), func() chroma.Rules {
		rules := vue.MustRules().Clone()
		for i, rule := range rules["tag"] {
			if strings.HasPrefix(rule.Pattern, `(v-[\w-]+)(=".+`) {
				rules["tag"][i].Pattern = strings.Replace(rule.Pattern, `=".+`, `="[^"]*`, 1)
			}
		}
		return rules
	}))
}
